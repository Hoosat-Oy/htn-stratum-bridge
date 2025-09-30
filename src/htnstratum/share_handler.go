package htnstratum

import (
	"fmt"
	"log"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hoosat-Oy/HTND/app/appmessage"
	"github.com/Hoosat-Oy/HTND/domain/consensus/model/externalapi"

	"github.com/Hoosat-Oy/HTND/domain/consensus/utils/consensushashing"
	"github.com/Hoosat-Oy/HTND/domain/consensus/utils/pow"
	"github.com/Hoosat-Oy/HTND/infrastructure/network/rpcclient"
	"github.com/Hoosat-Oy/htn-stratum-bridge/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

const varDiffThreadSleep = 5

type WorkStats struct {
	BlocksFound        atomic.Int64
	SharesFound        atomic.Int64
	SharesDiff         atomic.Float64
	StaleShares        atomic.Int64
	InvalidShares      atomic.Int64
	WorkerName         string
	StartTime          time.Time
	LastShare          time.Time
	VarDiffStartTime   time.Time
	VarDiffSharesFound atomic.Int64
	VarDiffWindow      int
	MinDiff            atomic.Float64
}

type shareHandler struct {
	hoosat       *rpcclient.RPCClient
	state        *MiningState
	soloDiff     float64
	stats        map[string]*WorkStats
	statsLock    sync.Mutex
	overall      WorkStats
	tipBlueScore uint64
}

type BanInfo struct {
	Address string
	Times   int
}

var bans = []BanInfo{}

const bps = 5

func AddressBanned(address string) bool {
	for _, ban := range bans {
		if ban.Address == address {
			if ban.Times > 10 {
				return true
			} else {
				return false
			}
		}
	}
	return false
}

func TryToBan(address string) {
	for i, ban := range bans {
		if ban.Address == address {
			bans[i].Times++
			return
		}
	}
	bans = append(bans, BanInfo{Address: address, Times: 1})
}

func newShareHandler(hoosat *rpcclient.RPCClient) *shareHandler {
	return &shareHandler{
		hoosat:    hoosat,
		stats:     map[string]*WorkStats{},
		statsLock: sync.Mutex{},
	}
}

func (sh *shareHandler) getCreateStats(ctx *gostratum.StratumContext) *WorkStats {
	sh.statsLock.Lock()
	var stats *WorkStats
	found := false
	if ctx.WorkerName != "" {
		stats, found = sh.stats[ctx.WorkerName]
	}
	if !found { // no worker name, check by remote address
		stats, found = sh.stats[ctx.RemoteAddr]
		if found {
			// no worker name, but remote addr is there
			// so replacet the remote addr with the worker names
			if ctx.WorkerName != "" {
				delete(sh.stats, ctx.RemoteAddr)
				stats.WorkerName = ctx.WorkerName
				sh.stats[ctx.WorkerName] = stats
			}
		}
	}
	if !found { // legit doesn't exist, create it
		stats = &WorkStats{}
		stats.LastShare = time.Now()
		stats.WorkerName = ctx.RemoteAddr
		stats.StartTime = time.Now()
		sh.stats[ctx.RemoteAddr] = stats

		// TODO: not sure this is the best place, nor whether we shouldn't be
		// resetting on disconnect
		InitWorkerCounters(ctx)
	}

	sh.statsLock.Unlock()
	return stats
}

type submitInfo struct {
	jobId    int64
	block    *appmessage.RPCBlock
	state    *MiningState
	noncestr string
	nonceVal uint64
	powHash  *externalapi.DomainHash
}

// ToBig converts a externalapi.DomainHash into a big.Int treated as a little endian string.
func toBig(hash *externalapi.DomainHash) *big.Int {
	// We treat the Hash as little-endian for PoW purposes, but the big package wants the bytes in big-endian, so reverse them.
	buf := hash.ByteSlice()
	blen := len(buf)
	for i := 0; i < blen/2; i++ {
		buf[i], buf[blen-1-i] = buf[blen-1-i], buf[i]
	}
	return new(big.Int).SetBytes(buf)
}

func validateSubmit(ctx *gostratum.StratumContext, event gostratum.JsonRpcEvent) (*submitInfo, error) {
	if len(event.Params) < 4 {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("malformed event, expected at least 4 params")
	}
	jobIdStr, ok := event.Params[1].(string)
	if !ok {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected type for param 1: %+v", event.Params...)
	}
	jobId, err := strconv.ParseInt(jobIdStr, 10, 0)
	if err != nil {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, errors.Wrap(err, "job id is not parsable as an number")
	}
	state := GetMiningState(ctx)
	block, exists := state.GetJob(int(jobId))
	if !exists {
		RecordWorkerError(ctx.WalletAddr, ErrMissingJob)
		return nil, fmt.Errorf("job does not exist. stale?")
	}
	noncestr, ok := event.Params[2].(string)
	if !ok {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected type for param 2: %+v", event.Params...)
	}
	powNumStr, ok := event.Params[3].(string)
	if !ok {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected type for param 3: %+v", event.Params...)
	}
	powHash, err := externalapi.NewDomainHashFromString(strings.Replace(powNumStr, "0x", "", 1))
	if err != nil {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected error for param 3: %w", err)
	}

	return &submitInfo{
		jobId:    jobId,
		state:    state,
		block:    block,
		noncestr: strings.Replace(noncestr, "0x", "", 1),
		powHash:  powHash,
	}, nil
}

var (
	ErrStaleShare = fmt.Errorf("stale share")
	ErrDupeShare  = fmt.Errorf("duplicate share")
)

// the max difference between tip blue score and job blue score that we'll accept
// anything greater than this is considered a stale
const workWindow = 8

func (sh *shareHandler) checkStales(ctx *gostratum.StratumContext, si *submitInfo) error {
	tip := sh.tipBlueScore
	if si.block.Header.BlueScore > tip {
		sh.tipBlueScore = si.block.Header.BlueScore
		return nil // can't be stale
	}
	if tip-si.block.Header.BlueScore > workWindow {
		RecordStaleShare(ctx)
		return errors.Wrapf(ErrStaleShare, "blueScore %d vs %d", si.block.Header.BlueScore, tip)
	}
	return nil
}

func (sh *shareHandler) setSoloDiff(diff float64) {
	sh.soloDiff = diff
}

func (sh *shareHandler) HandleSubmit(ctx *gostratum.StratumContext, event gostratum.JsonRpcEvent, soloMining bool) error {
	state := GetMiningState(ctx)
	submitInfo, err := validateSubmit(ctx, event)
	if err != nil {
		if err.Error() == "job does not exist. stale?" {
			RecordStaleShare(ctx)
			return ctx.ReplyStaleShare(event.Id)
		}
		return ctx.ReplyIncorrectData(event.Id)
	}

	// I have to ask why rdugan and brandon are modifying the miners nonce after submission.
	// I've been idiot for not commenting this gem out.
	// add extranonce to noncestr if enabled and submitted nonce is shorter than
	// expected (16 - <extranonce length> characters)
	// if ctx.Extranonce != "" {
	// 	extranonce2Len := 16 - len(ctx.Extranonce)
	// 	if len(submitInfo.noncestr) <= extranonce2Len {
	// 		submitInfo.noncestr = ctx.Extranonce + fmt.Sprintf("%0*s", extranonce2Len, submitInfo.noncestr)
	// 	}
	// }

	//ctx.Logger.Debug(submitInfo.block.Header.BlueScore, " submit ", submitInfo.noncestr)
	if state.useBigJob {
		submitInfo.nonceVal, err = strconv.ParseUint(submitInfo.noncestr, 16, 64)
		if err != nil {
			RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
			return errors.Wrap(err, "failed parsing noncestr")
		}
	} else {
		submitInfo.nonceVal, err = strconv.ParseUint(submitInfo.noncestr, 16, 64)
		if err != nil {
			RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
			return errors.Wrap(err, "failed parsing noncestr")
		}
	}
	stats := sh.getCreateStats(ctx)
	if err := sh.checkStales(ctx, submitInfo); err != nil {
		// remove job since it is bad job, so the job won't be reused for submit.
		state := GetMiningState(ctx)
		state.RemoveJob(int(submitInfo.jobId))
		if errors.Is(err, ErrStaleShare) {
			stats.StaleShares.Add(1)
			sh.overall.StaleShares.Add(1)
			RecordStaleShare(ctx)
			return ctx.ReplyStaleShare(event.Id)
		}
		// unknown error somehow
		ctx.Logger.Error("unknown error during check stales")
		RecordInvalidShare(ctx)
		return ctx.ReplyBadShare(event.Id)
	}

	// jsonHeader, err := json.MarshalIndent(submitInfo.block, "", "\t")
	// if err != nil {
	// 	return fmt.Errorf("failed to marshal submitted block: %+v", err)
	// }
	// fmt.Printf("Block: %s\n", jsonHeader)
	converted, err := appmessage.RPCBlockToDomainBlock(submitInfo.block, submitInfo.powHash.String())
	if err != nil {
		RecordInvalidShare(ctx)
		return ctx.ReplyIncorrectData(event.Id)
	}
	mutableHeader := converted.Header.ToMutable()
	mutableHeader.SetNonce(submitInfo.nonceVal)

	immutableHeader := mutableHeader.ToImmutable()
	converted.Header = immutableHeader

	powState := pow.NewState(mutableHeader)
	// fmt.Printf("Block version: %d\n", powState.BlockVersion)
	// fmt.Printf("Block prevHeader: %s\n", powState.PrevHeader.String())
	recalculatedPowNum, _ := powState.CalculateProofOfWorkValue()
	submittedPowNum := toBig(submitInfo.powHash)

	// The block hash must be less or equal than the claimed target.
	//fmt.Printf("%s\r\n", submittedPowNum)
	//fmt.Printf("%s\r\n", recalculatedPowNum)
	if submittedPowNum.Cmp(recalculatedPowNum) == 0 {
		if recalculatedPowNum.Cmp(&powState.Target) <= 0 {
			if err := sh.submit(ctx, converted, submitInfo, event.Id); err != nil {
				if strings.Contains(err.Error(), "ErrDuplicateBlock") {
					ctx.Logger.Warn("block rejected, duplicate")
					stats.StaleShares.Add(1)
					sh.overall.StaleShares.Add(1)
					RecordDupeShare(ctx)
					return ctx.ReplyDupeShare(event.Id)
				} else if strings.Contains(err.Error(), "ErrInvalidPoW") {
					ctx.Logger.Warn("block rejected, incorred pow")
					stats.StaleShares.Add(1)
					sh.overall.InvalidShares.Add(1)
					RecordInvalidShare(ctx)
					return ctx.ReplyIncorrectPow(event.Id)
				} else {
					ctx.Logger.Warn("block rejected, unknown issue", zap.Error(err))
					stats.InvalidShares.Add(1)
					sh.overall.InvalidShares.Add(1)
					RecordInvalidShare(ctx)
					return ctx.ReplyBadShare(event.Id)
				}
			}
		} else if recalculatedPowNum.Cmp(state.stratumDiff.targetValue) >= 0 {
			if soloMining {
				ctx.Logger.Warn("weak block")
			} else {
				ctx.Logger.Warn("weak share")
			}
			stats.InvalidShares.Add(1)
			sh.overall.InvalidShares.Add(1)
			RecordWeakShare(ctx)
			return ctx.ReplyLowDiffShare(event.Id)
		}
	} else {
		stats.InvalidShares.Add(1)
		sh.overall.InvalidShares.Add(1)
		RecordInvalidShare(ctx)
		return ctx.ReplyIncorrectPow(event.Id)
	}

	blockHash := consensushashing.BlockHash(converted).String()
	stats.SharesFound.Add(1)
	stats.SharesDiff.Add(state.stratumDiff.hashValue)
	stats.LastShare = time.Now()
	sh.overall.SharesFound.Add(1)
	RecordShareFound(ctx, state.stratumDiff.hashValue)
	stats.BlocksFound.Add(1)
	sh.overall.BlocksFound.Add(1)
	RecordBlockFound(ctx, converted.Header.Nonce(), converted.Header.BlueScore(), blockHash)
	// ctx.ReplySuccess(event.Id)
	return ctx.ReplySuccessWithBlockInfo(event.Id, true, blockHash)
	return nil
}

func (sh *shareHandler) submit(ctx *gostratum.StratumContext,
	block *externalapi.DomainBlock, submitInfo *submitInfo, eventId any) error {
	mutable := block.Header.ToMutable()
	mutable.SetNonce(submitInfo.nonceVal)
	block = &externalapi.DomainBlock{
		Header:       mutable.ToImmutable(),
		Transactions: block.Transactions,
	}
	state := GetMiningState(ctx)
	_, err := sh.hoosat.SubmitBlock(block, submitInfo.powHash.String())
	state.RemoveJob(int(submitInfo.jobId))
	return err
}

func (sh *shareHandler) startStatsThread() error {
	start := time.Now()
	for {
		// console formatting is terrible. Good luck whever touches anything
		time.Sleep(10 * time.Second)
		sh.statsLock.Lock()
		str := "\n===============================================================================\n"
		str += "  worker name   |  avg hashrate  |   acc/stl/inv  |    blocks    |    uptime   \n"
		str += "-------------------------------------------------------------------------------\n"
		var lines []string
		totalRate := float64(0)
		for _, v := range sh.stats {
			rate := GetAverageHashrateGHs(v)
			totalRate += rate
			rateStr := stringifyHashrate(rate)
			ratioStr := fmt.Sprintf("%d/%d/%d", v.SharesFound.Load(), v.StaleShares.Load(), v.InvalidShares.Load())
			lines = append(lines, fmt.Sprintf(" %-15s| %14.14s | %14.14s | %12d | %11s",
				v.WorkerName, rateStr, ratioStr, v.BlocksFound.Load(), time.Since(v.StartTime).Round(time.Second)))
		}
		sort.Strings(lines)
		str += strings.Join(lines, "\n")
		rateStr := stringifyHashrate(totalRate)
		ratioStr := fmt.Sprintf("%d/%d/%d", sh.overall.SharesFound.Load(), sh.overall.StaleShares.Load(), sh.overall.InvalidShares.Load())
		str += "\n-------------------------------------------------------------------------------\n"
		str += fmt.Sprintf("                | %14.14s | %14.14s | %12d | %11s",
			rateStr, ratioStr, sh.overall.BlocksFound.Load(), time.Since(start).Round(time.Second))
		str += "\n-------------------------------------------------------------------------------\n"
		str += " Est. Network Hashrate: " + stringifyHashrate(DiffToHash(sh.soloDiff)*bps) + "\n"
		str += " Mining difficulty:     " + fmt.Sprintf("%f", sh.soloDiff)
		str += "\n========================================================== htn_bridge_" + version + " ===\n"
		sh.statsLock.Unlock()
		log.Println(str)
	}
}

func GetAverageHashrateGHs(stats *WorkStats) float64 {
	return stats.SharesDiff.Load() / time.Since(stats.StartTime).Seconds()
}

func stringifyHashrate(ghs float64) string {
	unitStrings := [...]string{"", "K", "M", "G", "T", "P", "E", "Z", "Y"}
	var unit string
	var hr float64

	if ghs*1000000 < 1 {
		hr = ghs * 1000 * 1000 * 1000
		unit = unitStrings[0]
	} else if ghs*1000 < 1 {
		hr = ghs * 1000 * 1000
		unit = unitStrings[1]
	} else if ghs < 1 {
		hr = ghs * 1000
		unit = unitStrings[2]
	} else if ghs < 1000 {
		hr = ghs
		unit = unitStrings[3]
	} else {
		for i, u := range unitStrings[4:] {
			hr = ghs / (float64(i) * 1000)
			if hr < 1000 {
				break
			}
			unit = u
		}
	}

	formatted := fmt.Sprintf("%0.2f", hr)
	formatted = strings.Replace(formatted, ".", ",", 1)

	return fmt.Sprintf("%s%sH/s", formatted, unit)
}

func (sh *shareHandler) startVardiffThread(expectedShareRate uint, logStats bool) error {
	// 15 shares/min allows a ~95% confidence assumption of:
	//   < 100% variation after 1m
	//   < 50% variation after 3m
	//   < 25% variation after 10m
	//   < 15% variation after 30m
	//   < 10% variation after 1h
	//   < 5% variation after 4h
	var windows = [...]uint{1, 3, 10, 30, 60, 240, 0}
	var tolerances = [...]float64{1, 0.5, 0.25, 0.15, 0.1, 0.05, 0.05}

	for {
		time.Sleep(varDiffThreadSleep * time.Second)

		// don't like locking entire stats struct - risk should be negligible
		// if mutex is ultimately needed, should move to one per client
		// sh.statsLock.Lock()

		stats := "\n=== vardiff ===================================================================\n\n"
		stats += "  worker name  |    diff     |  window  |  elapsed   |    shares   |   rate    \n"
		stats += "-------------------------------------------------------------------------------\n"

		var statsLines []string
		var toleranceErrs []string

		for _, v := range sh.stats {
			if v.VarDiffStartTime.IsZero() {
				// no vardiff sent to client
				continue
			}

			worker := v.WorkerName
			diff := v.MinDiff.Load()
			shares := v.VarDiffSharesFound.Load()
			duration := time.Since(v.VarDiffStartTime).Minutes()
			shareRate := float64(shares) / duration
			shareRateRatio := shareRate / float64(expectedShareRate)
			window := windows[v.VarDiffWindow]
			tolerance := tolerances[v.VarDiffWindow]

			statsLines = append(statsLines, fmt.Sprintf(" %-14s| %11.2f | %8d | %10.2f | %11d | %9.2f", worker, diff, window, duration, shares, shareRate))

			// check final stage first, as this is where majority of time spent
			if window == 0 {
				if math.Abs(1-shareRateRatio) >= tolerance {
					// final stage submission rate OOB
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s final share rate (%f) exceeded tolerance (+/- %d%%)", worker, shareRate, int(tolerance*100)))
					updateVarDiff(v, diff*shareRateRatio)
				}
				continue
			}

			// check all previously cleared windows
			i := 1
			for i < v.VarDiffWindow {
				if math.Abs(1-shareRateRatio) >= tolerances[i] {
					// breached tolerance of previously cleared window
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded tolerance (+/- %d%%) for %dm window", worker, shareRate, int(tolerances[i]*100), windows[i]))
					updateVarDiff(v, diff*shareRateRatio)
					break
				}
				i++
			}
			if i < v.VarDiffWindow {
				// should only happen if we broke previous loop
				continue
			}

			// check for current window max exception
			if float64(shares) >= float64(window*expectedShareRate)*(1+tolerance) {
				// submission rate > window max
				toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded upper tolerance (+ %d%%) for %dm window", worker, shareRate, int(tolerance*100), window))
				updateVarDiff(v, diff*shareRateRatio)
				continue
			}

			// check whether we've exceeded window length
			if duration >= float64(window) {
				// check for current window min exception
				if float64(shares) <= float64(window*expectedShareRate)*(1-tolerance) {
					// submission rate < window min
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded lower tolerance (- %d%%) for %dm window", worker, shareRate, int(tolerance*100), window))
					updateVarDiff(v, diff*math.Max(shareRateRatio, 0.1))
					continue
				}

				v.VarDiffWindow++
			}
		}
		sort.Strings(statsLines)
		stats += strings.Join(statsLines, "\n")
		stats += "\n\n======================================================== htn_bridge_" + version + " ===\n"
		stats += strings.Join(toleranceErrs, "\n")
		if logStats {
			log.Println(stats)
		}

		// sh.statsLock.Unlock()
	}
}

// update vardiff with new mindiff, reset counters, and disable tracker until
// client handler restarts it while sending diff on next block
func updateVarDiff(stats *WorkStats, minDiff float64) float64 {
	stats.VarDiffStartTime = time.Time{}
	stats.VarDiffWindow = 0
	previousMinDiff := stats.MinDiff.Load()
	stats.MinDiff.Store(minDiff)
	return previousMinDiff
}

// (re)start vardiff tracker
func startVarDiff(stats *WorkStats) {
	if stats.VarDiffStartTime.IsZero() {
		stats.VarDiffSharesFound.Store(0)
		stats.VarDiffStartTime = time.Now()
	}
}

func (sh *shareHandler) startClientVardiff(ctx *gostratum.StratumContext) {
	stats := sh.getCreateStats(ctx)
	startVarDiff(stats)
}

func (sh *shareHandler) getClientVardiff(ctx *gostratum.StratumContext) float64 {
	stats := sh.getCreateStats(ctx)
	return stats.MinDiff.Load()
}

func (sh *shareHandler) setClientVardiff(ctx *gostratum.StratumContext, minDiff float64) float64 {
	stats := sh.getCreateStats(ctx)
	previousMinDiff := updateVarDiff(stats, math.Max(minDiff, 0.00001))
	startVarDiff(stats)
	return previousMinDiff
}
