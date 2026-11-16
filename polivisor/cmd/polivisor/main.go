// Command polivisor supervises a coupled set of local services (PM2-like, but
// dependency-aware and memory-capped). The launcher writes the stack config and
// runs `polivisor run --config <file>`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/polivisor/internal/spec"
	"github.com/raulbattistini3-sys/erp-v1/polivisor/internal/supervisor"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  polivisor run    --config polivisor.json   start the stack and supervise it
  polivisor check  --config polivisor.json   validate config and print start tiers
  polivisor status [--addr 127.0.0.1:9900]   query a running polivisor`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	cfgPath := fs.String("config", "polivisor.json", "stack config")
	addr := fs.String("addr", "127.0.0.1:9900", "control address (status)")
	fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "run":
		os.Exit(run(*cfgPath))
	case "check":
		cfg, err := spec.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		tiers, _ := cfg.Tiers()
		for i, t := range tiers {
			fmt.Printf("tier %d: %v\n", i, t)
		}
	case "status":
		os.Exit(status(*addr))
	default:
		usage()
	}
}

func run(path string) int {
	cfg, err := spec.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "polivisor:", err)
		return 1
	}
	sv, err := supervisor.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "polivisor:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "polivisor:", err)
		return 1
	}
	return 0
}

func status(addr string) int {
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://" + addr + "/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "polivisor not reachable:", err)
		return 1
	}
	defer r.Body.Close()
	var body struct {
		Available int                 `json:"available_mb"`
		Services  []supervisor.Status `json:"services"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%-10s %-4s %-9s %7s %8s %8s %s\n", "SERVICE", "TIER", "STATE", "PID", "MEM", "CAP", "RESTARTS")
	for _, s := range body.Services {
		lim := "-"
		if s.LimitMB > 0 {
			lim = fmt.Sprintf("%dM", s.LimitMB)
		}
		fmt.Printf("%-10s %-4d %-9s %7d %7dM %8s %d\n", s.Name, s.Tier, s.State, s.PID, s.MemMB, lim, s.Restarts)
	}
	fmt.Printf("free RAM: %d MB\n", body.Available)
	return 0
}
