package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"time"

	htnstratum "github.com/Hoosat-Oy/htn-stratum-bridge/src/htnstratum"
	"gopkg.in/yaml.v2"
)

func main() {
	pwd, _ := os.Getwd()
	fullPath := path.Join(pwd, "config.yaml")
	log.Printf("loading config @ `%s`", fullPath)
	rawCfg, err := ioutil.ReadFile(fullPath)
	if err != nil {
		log.Printf("config file not found: %s", err)
		os.Exit(1)
	}
	cfg := htnstratum.BridgeConfig{}
	if err := yaml.Unmarshal(rawCfg, &cfg); err != nil {
		log.Printf("failed parsing config file: %s", err)
		os.Exit(1)
	}

	// override flag.Usage for better help output.
	flag.Usage = func() {
		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stderr, "  -%v %v\n", f.Name, f.Value)
			fmt.Fprintf(os.Stderr, "    	%v (default \"%v\")\n", f.Usage, f.Value)
		})
	}

	flag.StringVar(&cfg.StratumPort, "stratum", cfg.StratumPort, "stratum port to listen on")
	flag.BoolVar(&cfg.PrintStats, "stats", cfg.PrintStats, "true to show periodic stats to console")
	flag.StringVar(&cfg.RPCServer, "hoosat_address", cfg.RPCServer, "address of the spectred node")
	flag.DurationVar(&cfg.BlockWaitTime, "blockwait", cfg.BlockWaitTime, "time in ms to wait before manually requesting new block")
	flag.Float64Var(&cfg.MinShareDiff, "mindiff", cfg.MinShareDiff, "minimum share difficulty to accept from miner(s)")
	flag.BoolVar(&cfg.VarDiff, "vardiff", cfg.VarDiff, "true to enable auto-adjusting variable min diff")
	flag.UintVar(&cfg.SharesPerMin, "sharespermin", cfg.SharesPerMin, "number of shares per minute the vardiff engine should target")
	flag.BoolVar(&cfg.VarDiffStats, "vardiffstats", cfg.VarDiffStats, "include vardiff stats readout every 10s in log")
	flag.BoolVar(&cfg.SoloMining, "solo", cfg.SoloMining, "true to use network diff instead of stratum vardiff")
	flag.UintVar(&cfg.ExtranonceSize, "extranonce", cfg.ExtranonceSize, "size in bytes of extranonce")
	flag.StringVar(&cfg.PromPort, "prom", cfg.PromPort, "address to serve prom stats")
	flag.BoolVar(&cfg.UseLogFile, "log", cfg.UseLogFile, "if true will output errors to log file")
	flag.StringVar(&cfg.HealthCheckPort, "hcp", cfg.HealthCheckPort, "(rarely used) if defined will expose a health check on /readyz")
	flag.BoolVar(&cfg.MineWhenNotSynced, "minewhennotsynced", cfg.MineWhenNotSynced, "mine when not synced")
	flag.Parse()

	if cfg.MinShareDiff == 0 {
		cfg.MinShareDiff = 4
	}
	if cfg.BlockWaitTime == 0 {
		cfg.BlockWaitTime = 200 * time.Millisecond // this should never happen due to HTN 200ms block times
	}

	log.Println("----------------------------------")
	log.Printf("initializing bridge")
	log.Printf("hoosat:\t\t\t%s", cfg.RPCServer)
	log.Printf("stratum:\t\t\t%s", cfg.StratumPort)
	log.Printf("prom:\t\t\t%s", cfg.PromPort)
	log.Printf("stats:\t\t\t%t", cfg.PrintStats)
	log.Printf("log:\t\t\t%t", cfg.UseLogFile)
	log.Printf("min diff:\t\t\t%.10f", cfg.MinShareDiff)
	log.Printf("var diff:\t\t\t%t", cfg.VarDiff)
	log.Printf("shares per min:\t\t%d", cfg.SharesPerMin)
	log.Printf("var diff stats:\t\t%t", cfg.VarDiffStats)
	log.Printf("solo mining:\t\t%t", cfg.SoloMining)
	log.Printf("block wait:\t\t\t%s", cfg.BlockWaitTime)
	log.Printf("extranonce size:\t\t%d", cfg.ExtranonceSize)
	log.Printf("health check:\t\t\t%s", cfg.HealthCheckPort)
	log.Printf("Mine when not synced:\t%t", cfg.MineWhenNotSynced)
	log.Println("----------------------------------")

	if err := htnstratum.ListenAndServe(cfg); err != nil {
		log.Println(err)
	}
}
