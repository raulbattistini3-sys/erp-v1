// erp-cache: a read cache in front of the ERP API and the TCC telemetry server.
//
//	erp-cache [-config cache.json]                 serve every configured site
//	erp-cache triggers -site erp [-apply]          print (or run) the SQL that feeds a site
//
// See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/raulbattistini3-sys/erp-cache/internal/coldstart"
	"github.com/raulbattistini3-sys/erp-cache/internal/config"
	"github.com/raulbattistini3-sys/erp-cache/internal/feed"
	"github.com/raulbattistini3-sys/erp-cache/internal/network"
	"github.com/raulbattistini3-sys/erp-cache/internal/proxy"
	"github.com/raulbattistini3-sys/erp-cache/internal/report"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level()}))
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	path := fs.String("config", "", "config file (default $CACHE_CONFIG, else cache.json)")
	siteName := fs.String("site", "", "triggers: the site")
	apply := fs.Bool("apply", false, "triggers: run the SQL on the site's database instead of printing it")
	fs.Parse(args)

	explicit := *path != ""
	if !explicit {
		*path = os.Getenv("CACHE_CONFIG")
		explicit = *path != ""
	}
	if !explicit {
		*path = "cache.json"
	}
	cfg, err := config.Load(*path, explicit)
	if err != nil {
		fatal(log, "config", err)
	}

	switch cmd {
	case "serve":
		if err := serve(cfg, log); err != nil {
			fatal(log, "serve", err)
		}
	case "triggers":
		if err := triggers(cfg, *siteName, *apply); err != nil {
			fatal(log, "triggers", err)
		}
	default:
		fatal(log, "usage", fmt.Errorf("unknown command %q (serve | triggers)", cmd))
	}
}

func serve(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sites []*proxy.Site
	var feeds []*feed.Feed
	type dbInfo struct{ url, ca string }
	dbs := make([]dbInfo, len(cfg.Sites))
	for i, sc := range cfg.Sites {
		dbURL := ""
		if sc.DatabaseURLEnv != "" {
			dbURL = os.Getenv(sc.DatabaseURLEnv)
			if dbURL == "" {
				log.Warn("no database URL: this site only bypasses until it is set", "site", sc.Name, "env", sc.DatabaseURLEnv)
			}
		} else {
			log.Warn("no feed configured: entries are only dropped by TTL", "site", sc.Name)
		}
		s, err := proxy.New(sc, sc.DatabaseURLEnv != "", log)
		if err != nil {
			return err
		}
		sites = append(sites, s)
		if dbURL != "" {
			ca := ""
			if sc.DatabaseCACertEnv != "" {
				ca = os.Getenv(sc.DatabaseCACertEnv)
			}
			dbs[i] = dbInfo{dbURL, ca}
			feeds = append(feeds, &feed.Feed{URL: dbURL, CACert: ca, Channel: sc.Channel, Target: s, Log: log.With("site", sc.Name)})
		}
	}

	host, _ := os.Hostname()
	instance := cfg.Telemetry.InstanceID
	if instance == "" {
		instance = host
	}
	token := ""
	if cfg.Telemetry.TokenEnv != "" {
		token = os.Getenv(cfg.Telemetry.TokenEnv)
	}
	rep := report.New(cfg.Telemetry.URL, token, cfg.Telemetry.Source, instance, cfg.Telemetry.Interval.D(), sites, log)

	var wg sync.WaitGroup
	for _, f := range feeds {
		f.OnState = func(up bool, err error) {
			if up {
				rep.Lifecycle("recovered", f.Channel+" feed up")
			} else {
				rep.Lifecycle("degraded", fmt.Sprintf("%s feed down: %v", f.Channel, err))
			}
		}
		wg.Go(func() { f.Run(ctx) })
	}

	var servers []*http.Server
	errc := make(chan error, len(sites)+1)
	for i, s := range sites {
		srv := &http.Server{Addr: cfg.Sites[i].Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() { errc <- listen(srv) }()
		log.Info("site listening", "site", s.Name, "addr", srv.Addr, "upstream", cfg.Sites[i].Upstream)
	}
	// sites with "register" join the network registry, so the gateway can route a prefix through them
	if nc, ok := network.FromEnv(); ok {
		for _, sc := range cfg.Sites {
			if sc.Register == "" {
				continue
			}
			reg := network.Registration{Address: "http://" + advertise(sc.Listen), Meta: map[string]string{"cache": sc.Name, "upstream": sc.Upstream}}
			id := network.InstanceID(sc.Register)
			wg.Go(func() { nc.Keep(ctx, sc.Register, id, reg, 5*time.Second, log.With("site", sc.Name)) })
		}
	}
	ctl := &http.Server{Addr: cfg.Control, Handler: control(sites), ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, ctl)
	go func() { errc <- listen(ctl) }()
	log.Info("control listening", "addr", cfg.Control)

	rep.Lifecycle("ready", "")
	repDone := make(chan struct{})
	go func() { rep.Run(ctx); close(repDone) }()

	// cold-start checks, beside serving (never in front of it): each database's first connection,
	// SELECT 1 and first read of every table of its current schema, then each upstream's warm-up paths
	for i, sc := range cfg.Sites {
		db := dbs[i]
		wg.Go(func() { coldCheck(ctx, sc, db.url, db.ca, rep, log.With("site", sc.Name)) })
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
		stop()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(sctx)
	}
	wg.Wait()
	<-repDone
	return err
}

// coldCheck runs the site's cold-start measurements, logs them and reports them as one "cold-start"
// sample. A missing table is a warning: cache.json names a table the migrations do not create.
func coldCheck(ctx context.Context, sc config.Site, dbURL, ca string, rep *report.Reporter, log *slog.Logger) {
	sample := map[string]any{"type": "cold-start", "category": "performance", "name": sc.Name}
	if dbURL != "" {
		db := coldstart.CheckDB(ctx, dbURL, ca, sc.Tables())
		sample["db"] = db
		switch {
		case db.Err != "":
			log.Warn("cold start: database check failed", "err", db.Err, "connectMs", db.ConnectMs)
		default:
			log.Info("cold start: database", "connectMs", db.ConnectMs, "select1Ms", db.Select1Ms,
				"tables", len(db.Tables), "tablesMs", db.TablesMs, "migrations", db.Migrations)
			if slowest := slowestTable(db.Tables); slowest != nil {
				log.Info("cold start: slowest first read", "table", slowest.Name, "ms", slowest.Ms)
			}
			for _, t := range db.Tables {
				if t.Err != "" {
					log.Warn("cold start: table read failed", "table", t.Name, "err", t.Err)
				}
			}
			if len(db.Missing) > 0 {
				log.Warn("cold start: cache.json names tables the schema does not have; their entries are never invalidated",
					"missing", db.Missing)
			}
		}
	}
	if len(sc.Warmup) > 0 {
		auth := ""
		if sc.WarmupAuthEnv != "" {
			auth = os.Getenv(sc.WarmupAuthEnv)
		}
		reqs, err := coldstart.Warm(ctx, sc.Upstream, sc.Warmup, auth, sc.WarmupWait.D())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("cold start: upstream warm-up skipped", "err", err)
			sample["warmupErr"] = err.Error()
		}
		for _, r := range reqs {
			log.Info("cold start: upstream", "path", r.Path, "status", r.Status, "coldMs", r.ColdMs, "warmMs", r.WarmMs, "err", r.Err)
		}
		sample["warmup"] = reqs
	}
	if len(sample) > 3 {
		rep.Sample(sample)
	}
}

func slowestTable(ts []coldstart.Table) *coldstart.Table {
	var s *coldstart.Table
	for i := range ts {
		if s == nil || ts[i].Ms > s.Ms {
			s = &ts[i]
		}
	}
	return s
}

// advertise is the address others reach a listen address at: NETWORK_ADVERTISE_HOST, or loopback for
// a wildcard host.
func advertise(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if h := strings.TrimSpace(os.Getenv("NETWORK_ADVERTISE_HOST")); h != "" {
		host = h
	} else if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func listen(srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s: %w", srv.Addr, err)
	}
	return nil
}

// control: GET /health (200 when every feed is up, 503 otherwise), GET /stats, POST /flush?site=
func control(sites []*proxy.Site) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]bool{}
		ok := true
		for _, s := range sites {
			out[s.Name] = s.FeedUp()
			ok = ok && s.FeedUp()
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			// degraded, not broken: requests still go through to the upstreams
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "feeds": out})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{}
		for _, s := range sites {
			out[s.Name] = map[string]any{"feedUp": s.FeedUp(), "bypassed": s.Bypassed(), "store": s.Store.Stats(), "dedupe": s.Dedupes()}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /flush", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("site")
		n := 0
		for _, s := range sites {
			if name == "" || s.Name == name {
				s.Store.ForgetAll()
				s.ForgetRecords()
				n++
			}
		}
		if n == 0 {
			http.Error(w, "no such site", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func triggers(cfg *config.Config, name string, apply bool) error {
	for _, s := range cfg.Sites {
		if s.Name != name {
			continue
		}
		sql := feed.Triggers(s.Channel, s.Tables())
		if !apply {
			fmt.Print(sql)
			return nil
		}
		url := os.Getenv(s.DatabaseURLEnv)
		if s.DatabaseURLEnv == "" || url == "" {
			return fmt.Errorf("site %s: set %s to the database URL", s.Name, s.DatabaseURLEnv)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, sql); err != nil {
			return err
		}
		fmt.Printf("triggers installed on %d tables for channel %s\n", len(s.Tables()), s.Channel)
		return nil
	}
	return fmt.Errorf("no site named %q", name)
}

func level() slog.Level {
	if os.Getenv("CACHE_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}
