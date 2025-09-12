package htnstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/Hoosat-Oy/HTND/app/appmessage"
	"github.com/Hoosat-Oy/HTND/infrastructure/network/rpcclient"
	"github.com/Hoosat-Oy/htn-stratum-bridge/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type HtnApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	hoosat        *rpcclient.RPCClient
	connected     bool
}

func NewHoosatAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*HtnApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &HtnApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "hoosatapi:"+address)),
		hoosat:        client,
		connected:     true,
	}, nil
}

func (htnApi *HtnApi) Start(ctx context.Context, cfg BridgeConfig, blockCb func()) {
	if !cfg.MineWhenNotSynced {
		htnApi.waitForSync(true)
	}
	go htnApi.startBlockTemplateListener(ctx, blockCb)
	go htnApi.startStatsThread(ctx)
}

func (htnApi *HtnApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			htnApi.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := htnApi.hoosat.GetBlockDAGInfo()
			if err != nil {
				htnApi.logger.Warn("failed to get network hashrate from hoosat, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := htnApi.hoosat.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				htnApi.logger.Warn("failed to get network hashrate from hoosat, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (htnApi *HtnApi) reconnect() error {
	if htnApi.hoosat != nil {
		return htnApi.hoosat.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(htnApi.address)
	if err != nil {
		return err
	}
	htnApi.hoosat = client
	return nil
}

func (htnApi *HtnApi) waitForSync(verbose bool) error {
	if verbose {
		htnApi.logger.Info("checking hoosat sync state")
	}
	for {
		clientInfo, err := htnApi.hoosat.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from hoosat @ %s", htnApi.address)
		}
		if clientInfo.IsSynced {
			break
		}
		htnApi.logger.Warn("HTN is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		htnApi.logger.Info("HTN synced, starting server")
	}
	return nil
}

func (htnApi *HtnApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := htnApi.hoosat.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		htnApi.logger.Error("fatal: failed to register for block notifications from hoosat")
	}

	ticker := time.NewTicker(htnApi.blockWaitTime)
	for {
		if err := htnApi.waitForSync(false); err != nil {
			htnApi.logger.Error("error checking hoosat sync state, attempting reconnect: ", err)
			if err := htnApi.reconnect(); err != nil {
				htnApi.logger.Error("error reconnecting to hoosat, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			htnApi.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(htnApi.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (htnApi *HtnApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := htnApi.hoosat.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via htn-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from hoosat")
	}
	return template, nil
}
