// src/htnstratum/miner_rewards.go
// Foztor 1st October 25.
// Exposes an http API endpoint
//  curl "http://127.0.0.1:5556/miner/rewards?limit=450&address=hoosat:qrm2jaklpf95t4a3kxm04zr27sayq9avvdwqcapm5c696qa3vj3lj3ye3nr9s&worker=Volta" 
//  Returns JSON like this in an array
//   {
//      "minedBlueHash": "f2fc18865187ef63d912d066aade9d1f2d86be466b1ba843ae4c9b9fea279cf7",
//      "worker": "Volta",
//      "rewardAtoms": 1551343503,
//      "paidByBlockHash": "083512d11ef37de3d234d15ba0c011986d2722308e807fe8dbb883fe001ec877",
//      "paidBlueScore": 72787561,
//      "paidAt": 1759322062398
//   }
//  Idea is that miners can call it so that they can work out how many blocks actually got a reward
//  
//  Requires health_check_port: :5556 in config.yaml
//  Also requires  the handler to be registered in  stratum_handler.go
//  See Snippet below

/*
   if cfg.HealthCheckPort != "" {
                logger.Info("enabling health check on port " + cfg.HealthCheckPort)
                http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
                        w.WriteHeader(http.StatusOK)
                })

                registerMinerRewardsHandlers(htnApi) // <- New Rewards handler

                go http.ListenAndServe(cfg.HealthCheckPort, nil)
        }

*/
package htnstratum

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	// "time"
	"unicode"

	"github.com/Hoosat-Oy/HTND/app/appmessage"
)

// RewardsRow is a single "we mined this blue and got paid" record.
type RewardsRow struct {
	// The block we mined (a mergeset blue in the paying block)
	MinedBlueHash string `json:"minedBlueHash"`
	// Worker parsed from that mined blue's coinbase payload
	Worker string `json:"worker"`
	// Amount this paying block attributed to us
	RewardAtoms uint64 `json:"rewardAtoms"`
	// The chain block that paid us (its coinbase pays our address)
	PaidByBlockHash string `json:"paidByBlockHash"`
	// Metadata from the paying block
	PaidBlueScore uint64 `json:"paidBlueScore"`
	PaidAtMS      int64  `json:"paidAt"` // ms since epoch
}

// Register the rewards endpoint
// GET /miner/rewards?address=<hoosat:..>&limit=400&startHash=<cursor>&worker=<WorkerName>
func registerMinerRewardsHandlers(api *HtnApi) {
	http.HandleFunc("/miner/rewards", func(w http.ResponseWriter, r *http.Request) {
		// start := time.Now()

		// Ensure we always return a response and log any panic root cause.
		defer func() {
			if rec := recover(); rec != nil {
				logError(api, fmt.Sprintf("panic in /miner/rewards: %v\n%s", rec, string(debug.Stack())))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()

		q := r.URL.Query()

		addr := strings.TrimSpace(q.Get("address"))
		if addr == "" {
			http.Error(w, "missing required query parameter: address", http.StatusBadRequest)
			return
		}
		limit := 400
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				if n > 2000 {
					n = 2000
				}
				limit = n
			}
		}
		startHash := strings.TrimSpace(q.Get("startHash"))
		workerFilter := strings.TrimSpace(q.Get("worker")) // optional

		// Fetch recent or incremental chain blocks
		var blocks []*appmessage.GetBlockResponseMessage
		var err error
		if startHash != "" {
			blocks, err = fetchAddedChainBlocksFrom(api, startHash, limit)
		} else {
			blocks, err = fetchRecentChainBlocks(api, limit)
		}
		if err != nil {
			logError(api, fmt.Sprintf("failed fetching chain blocks: %v", err))
			http.Error(w, "failed to fetch chain blocks", http.StatusBadGateway)
			return
		}

		// Transform to per-mined-blue rewards
		out := make([]RewardsRow, 0, len(blocks))
		for _, br := range blocks {
			// Per-block guard so one bad block doesn't kill the whole response
			rows, err := processPayingBlock(api, br, addr, workerFilter)
			if err != nil {
				logError(api, fmt.Sprintf("skip paying block due to error: %v", err))
				continue
			}
			out = append(out, rows...)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logError(api, fmt.Sprintf("json encode failed: %v", err))
			http.Error(w, "encode error", http.StatusInternalServerError)
			return
		}

		// logInfo(api, fmt.Sprintf("miner/rewards served addr=%s worker=%s limit=%d rows=%d latency=%s",
			// addr, workerFilter, limit, len(out), time.Since(start)))
	})
}

// Process a single chain block that might be paying our address.
// Returns rows (one per mined blue) or an error to skip this block.
func processPayingBlock(api *HtnApi, br *appmessage.GetBlockResponseMessage, addr string, workerFilter string) ([]RewardsRow, error) {
	defer func() {
		if rec := recover(); rec != nil {
			logError(api, fmt.Sprintf("panic in processPayingBlock: %v\n%s", rec, string(debug.Stack())))
		}
	}()

	if br == nil || br.Block == nil || br.Block.VerboseData == nil || !br.Block.VerboseData.IsChainBlock {
		return nil, nil
	}

	// Does this paying block's coinbase pay our address?
	ok, amount := coinbaseSumToAddress(br.Block, addr)
	if !ok || amount == 0 {
		return nil, nil
	}

	// Identify our mined blue blocks by presence of worker tag in payload
	mined := findMinedBluesFromMergeSet(api, br.Block.VerboseData.MergeSetBluesHashes)
	if len(mined) == 0 {
		// No mergeset blue has a worker tag; can't attribute robustly -> skip silently
		return nil, nil
	}

	// Split reward if multiple of our mined blues are paid in the same paying block
	portion := amount / uint64(len(mined))
	remainder := amount % uint64(len(mined))

	paidAt := int64(0)
	if br.Block.Header != nil {
		paidAt = int64(br.Block.Header.Timestamp)
	}
	paidBlueScore := br.Block.VerboseData.BlueScore
	payingHash := br.Block.VerboseData.Hash

	rows := make([]RewardsRow, 0, len(mined))
	assignedRemainder := false
	want := sanitizeWorkerIDRewards(workerFilter)

	for i, mb := range mined {
		// Optional worker filter
		if workerFilter != "" && sanitizeWorkerIDRewards(mb.Worker) != want {
			continue
		}

		amt := portion
		// Assign remainder to the first INCLUDED row (not just the first mined)
		if !assignedRemainder {
			// If we skipped some mined entries above due to worker filter, we still give remainder to
			// the first row that passes the filter to keep totals correct.
			amt += remainder
			assignedRemainder = true
		}

		rows = append(rows, RewardsRow{
			MinedBlueHash:   mb.Hash,
			Worker:          mb.Worker,
			RewardAtoms:     amt,
			PaidByBlockHash: payingHash,
			PaidBlueScore:   paidBlueScore,
			PaidAtMS:        paidAt,
		})

		_ = i // index kept in case of future diagnostics
	}

	// If worker filter was set but none matched, return no rows for this paying block.
	if workerFilter != "" && len(rows) == 0 {
		return nil, nil
	}

	return rows, nil
}

// minedBlue describes a mergeset blue block we believe we mined, with worker extracted from its payload.
type minedBlue struct {
	Hash   string
	Worker string
}

// findMinedBluesFromMergeSet fetches each mergeset-blue and returns those that have a worker tag in payload.
// We DO NOT require the blue's coinbase to pay our address (Hoosat pays in a later chain child).
func findMinedBluesFromMergeSet(api *HtnApi, hashes []string) []minedBlue {
	out := make([]minedBlue, 0, len(hashes))
	for _, h := range hashes {
		if len(h) != 64 {
			continue
		}
		br, err := api.hoosat.GetBlock(h, true)
		if err != nil || br == nil || br.Block == nil {
			continue
		}
		w := extractWorkerFromPayload(br.Block)
		if w == "" {
			continue
		}
		out = append(out, minedBlue{Hash: h, Worker: w})
	}
	return out
}

// ---------- Chain fetch (incremental via VSPC with fallback) ----------

// fetchAddedChainBlocksFrom returns chain blocks added since startHash using
// GetVirtualSelectedParentChainFromBlock(startHash, false). It fetches the
// corresponding blocks with transactions and returns them newest-first.
// Falls back to walking the selected-parent chain if VSPC is unavailable.
func fetchAddedChainBlocksFrom(api *HtnApi, startHash string, capLimit int) ([]*appmessage.GetBlockResponseMessage, error) {
	// Prefer VSPC: efficient incremental chain delta
	if startHash != "" {
		if resp, err := api.hoosat.GetVirtualSelectedParentChainFromBlock(startHash, false); err == nil && resp != nil {
			// Expect the typed response in the current SDK
			if v, ok := any(resp).(*appmessage.GetVirtualSelectedParentChainFromBlockResponseMessage); ok && v != nil && len(v.AddedChainBlockHashes) > 0 {
				hashes := v.AddedChainBlockHashes
				// Cap to most recent capLimit entries
				if capLimit > 0 && len(hashes) > capLimit {
					hashes = hashes[len(hashes)-capLimit:]
				}
				// Fetch blocks newest-first
				out := make([]*appmessage.GetBlockResponseMessage, 0, len(hashes))
				for i := len(hashes) - 1; i >= 0; i-- {
					h := hashes[i]
					br, err := api.hoosat.GetBlock(h, true) // include tx to parse coinbase
					if err != nil || br == nil || br.Block == nil || br.Block.VerboseData == nil {
						continue
					}
					out = append(out, br)
				}
				return out, nil
			}
		}
	}

	// Fallback: walk backward from a tip along selected-parent until we hit startHash or capLimit
	dag, err := api.hoosat.GetBlockDAGInfo()
	if err != nil {
		return nil, err
	}
	if len(dag.TipHashes) == 0 {
		return nil, fmt.Errorf("no tip hashes")
	}

	cur := dag.TipHashes[0]
	out := make([]*appmessage.GetBlockResponseMessage, 0, capLimit)

	for capLimit <= 0 || len(out) < capLimit {
		br, err := api.hoosat.GetBlock(cur, true)
		if err != nil || br == nil || br.Block == nil || br.Block.VerboseData == nil {
			break
		}
		// Stop once we reach the caller's cursor
		if br.Block.VerboseData.Hash == startHash {
			break
		}
		out = append(out, br)

		sp := br.Block.VerboseData.SelectedParentHash
		if sp == "" || len(sp) != 64 {
			break
		}
		cur = sp
	}

	return out, nil
}

// fetchRecentChainBlocks returns the last `limit` blocks along the selected-parent chain.
func fetchRecentChainBlocks(api *HtnApi, limit int) ([]*appmessage.GetBlockResponseMessage, error) {
	dag, err := api.hoosat.GetBlockDAGInfo()
	if err != nil {
		return nil, err
	}
	if len(dag.TipHashes) == 0 {
		return nil, fmt.Errorf("no tip hashes")
	}

	cur := dag.TipHashes[0]
	out := make([]*appmessage.GetBlockResponseMessage, 0, limit)

	for len(out) < limit {
		br, err := api.hoosat.GetBlock(cur, true) // include transactions
		if err != nil || br == nil || br.Block == nil || br.Block.VerboseData == nil {
			break
		}
		out = append(out, br)

		sp := br.Block.VerboseData.SelectedParentHash
		if sp == "" || len(sp) != 64 {
			break
		}
		cur = sp
	}

	return out, nil
}

// ---------- Helpers (self-contained; no external deps) ----------

func logError(api *HtnApi, msg string) {
	if api != nil && api.logger != nil {
		api.logger.Error(msg)
	}
	fmt.Fprintf(os.Stderr, "%s\n", msg)
}
func logInfo(api *HtnApi, msg string) {
	if api != nil && api.logger != nil {
		api.logger.Info(msg)
	}
}

// coinbaseSumToAddress scans the coinbase (transactions[0]) and sums outputs
// that pay the given Bech32 address (case-insensitive, HRP "hoosat:" / "hoosattest:" tolerated).
// Returns (true, sum) if at least one output matches; otherwise (false, 0).
func coinbaseSumToAddress(block *appmessage.RPCBlock, addr string) (bool, uint64) {
	if block == nil || len(block.Transactions) == 0 || block.Transactions[0] == nil {
		return false, 0
	}
	cb := block.Transactions[0]
	if len(cb.Outputs) == 0 {
		return false, 0
	}

	// Normalize target address: strip HRP and lowercase
	addr = strings.TrimSpace(addr)
	addr = strings.ToLower(stripHRPRewards(addr))

	var sum uint64
	var matched bool

	for _, o := range cb.Outputs {
		if o == nil || o.VerboseData == nil {
			continue
		}
		a := strings.ToLower(stripHRPRewards(o.VerboseData.ScriptPublicKeyAddress))
		if a == addr {
			sum += o.Amount // Amount is uint64 in the SDK
			matched = true
		}
	}
	return matched, sum
}

// stripHRPRewards strips "hoosat:" or "hoosattest:" if present.
func stripHRPRewards(in string) string {
	if strings.HasPrefix(in, "hoosat:") {
		return in[len("hoosat:"):]
	}
	if strings.HasPrefix(in, "hoosattest:") {
		return in[len("hoosattest:"):]
	}
	return in
}

// Regex: case-insensitive "as worker <name>" or "by worker <name>", where <name> allows [A-Za-z0-9._-]{1,32}
var reWorker = regexp.MustCompile(`(?i)\b(?:as|by)\s+worker\s+([A-Za-z0-9._-]{1,32})`)

// extractWorkerFromPayload extracts worker from the coinbase payload; supports both hex and raw payload.
// Returns "" if not found. This function is panic-safe and never slices by indexes from a different string.
func extractWorkerFromPayload(block *appmessage.RPCBlock) (out string) {
	defer func() {
		if rec := recover(); rec != nil {
			out = ""
		}
	}()
	if block == nil || len(block.Transactions) == 0 || block.Transactions[0] == nil {
		return ""
	}
	cb := block.Transactions[0]
	payload := cb.Payload
	if payload == "" {
		return ""
	}

	// Detect hex. If it looks like hex, decode; else treat as raw.
	asHex := len(payload)%2 == 0
	if asHex {
		for i := 0; i < len(payload); i++ {
			c := payload[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				asHex = false
				break
			}
		}
	}

	var raw []byte
	if asHex {
		if b, err := hex.DecodeString(payload); err == nil {
			raw = b
		}
	}
	if len(raw) == 0 {
		raw = []byte(payload)
	}

	s := string(raw)

	// Quick normalize: replace control whitespace with space to help regex matching against binary-ish payloads.
	builder := strings.Builder{}
	builder.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			builder.WriteByte(' ')
		default:
			// Keep printable ASCII; replace other controls with dot
			if r < 32 && r != ' ' {
				builder.WriteByte('.')
			} else {
				// Keep rune
				builder.WriteRune(r)
			}
		}
	}
	norm := builder.String()

	m := reWorker.FindStringSubmatch(norm)
	if len(m) >= 2 {
		// Trim and re-sanitize to allowed set defensively
		w := m[1]
		var b strings.Builder
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
				b.WriteRune(r)
				if b.Len() >= 32 {
					break
				}
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// sanitizeWorkerIDRewards normalizes worker strings for comparison: replace spaces with underscore and keep [A-Za-z0-9._-], max 32.
func sanitizeWorkerIDRewards(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, " ", "_")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			if b.Len() >= 32 {
				break
			}
		}
	}
	return strings.ToLower(b.String())
}
