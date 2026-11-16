// Package coldstart measures what the first requests after a (re)start pay for, before users do.
//
// There are several problems under the name "cold start", and each needs its own number:
//
//   - The database: the first connection (TCP, TLS, auth, backend fork) and the first read of each table
//     (catalog lookups, relation cache, pages not in shared_buffers yet). CheckDB connects, runs SELECT 1,
//     then SELECT 1 FROM every table the applied migrations created (whatever drizzle's current schema is,
//     read from pg_catalog, so nothing here goes stale when a migration adds a table), and reports any
//     table the cache's routes depend on that the schema does not have.
//   - The upstream process: module compilation (Node's compile cache helps on later starts), JIT tiering,
//     lazy pools and lazy imports. Warm requests each path twice straight at the upstream (bypassing the
//     cache, so nothing is stored) and reports the first and the second time; the difference is the cold
//     cost of that path. It is a controlled warm-up after readiness: the users' first search no longer
//     pays it.
//
// Neither fills the cache: entries are per user (Vary: Authorization) and must come from real requests.
package coldstart

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/raulbattistini3-sys/erp-cache/internal/feed"
)

// Table is one table's first read.
type Table struct {
	Name string  `json:"name"`
	Ms   float64 `json:"ms"`
	Err  string  `json:"err,omitempty"`
}

// DB is one database's cold numbers.
type DB struct {
	ConnectMs float64 `json:"connectMs"`
	Select1Ms float64 `json:"select1Ms"`
	// Migrations applied according to drizzle.__drizzle_migrations (-1: the table does not exist)
	Migrations int `json:"migrations"`
	// Tables: every base table of the public schema, in the order touched
	Tables []Table `json:"tables"`
	// TablesMs: all of them together
	TablesMs float64 `json:"tablesMs"`
	// Missing: tables the site's routes (or flushOn) name that the schema does not have; their entries
	// would never be dropped by a change, so a non-empty list means cache.json and the migrations disagree
	Missing []string `json:"missing,omitempty"`
	Err     string   `json:"err,omitempty"`
}

// CheckDB measures one database. expected are the tables the cache depends on (config.Site.Tables()).
func CheckDB(ctx context.Context, url, caCert string, expected []string) DB {
	out := DB{Migrations: -1}
	cfg, err := feed.ConnConfig(url, caCert)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	t := time.Now()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	out.ConnectMs = since(t)
	if err != nil {
		out.Err = "connect: " + err.Error()
		return out
	}
	defer conn.Close(context.Background())

	t = time.Now()
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		out.Err = "select 1: " + err.Error()
		return out
	}
	out.Select1Ms = since(t)

	// drizzle keeps its journal in drizzle.__drizzle_migrations; to_regclass is NULL when it is not there
	var applied *int
	if err := conn.QueryRow(ctx,
		`SELECT CASE WHEN to_regclass('drizzle.__drizzle_migrations') IS NULL THEN NULL
		        ELSE (SELECT count(*)::int FROM drizzle.__drizzle_migrations) END`).Scan(&applied); err == nil && applied != nil {
		out.Migrations = *applied
	}

	rows, err := conn.Query(ctx,
		`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  ORDER BY c.relname`)
	if err != nil {
		out.Err = "list tables: " + err.Error()
		return out
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		out.Err = "list tables: " + err.Error()
		return out
	}

	all := time.Now()
	have := map[string]bool{}
	for _, name := range names {
		have[name] = true
		t := time.Now()
		var x int
		err := conn.QueryRow(ctx, "SELECT 1 FROM "+pgx.Identifier{"public", name}.Sanitize()+" LIMIT 1").Scan(&x)
		tbl := Table{Name: name, Ms: since(t)}
		if err != nil && err != pgx.ErrNoRows { // an empty table is fine
			tbl.Err = err.Error()
		}
		out.Tables = append(out.Tables, tbl)
	}
	out.TablesMs = since(all)
	for _, name := range expected {
		if !have[name] {
			out.Missing = append(out.Missing, name)
		}
	}
	return out
}

// Request is one warm-up path: the first answer (cold) and the second (warm).
type Request struct {
	Path   string  `json:"path"`
	Status int     `json:"status"`
	ColdMs float64 `json:"coldMs"`
	WarmMs float64 `json:"warmMs"`
	Err    string  `json:"err,omitempty"`
}

// Warm waits (up to wait) for the upstream to answer at all, then requests each path twice. auth, when
// set, is sent as the Authorization header. Any status counts as an answer: a 401 still went through
// the process's routing, middleware and modules.
func Warm(ctx context.Context, upstream string, paths []string, auth string, wait time.Duration) ([]Request, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	get := func(path string) (int, float64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(upstream, "/")+path, nil)
		if err != nil {
			return 0, 0, err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		t := time.Now()
		res, err := client.Do(req)
		if err != nil {
			return 0, since(t), err
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode, since(t), nil
	}

	// readiness: the first path until anything answers (the upstream may still be starting)
	deadline := time.Now().Add(wait)
	for {
		ctxReq, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(ctxReq, http.MethodHead, strings.TrimRight(upstream, "/")+"/", nil)
		res, err := http.DefaultTransport.RoundTrip(req)
		cancel()
		if err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s did not answer within %s: %w", upstream, wait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	var out []Request
	for _, p := range paths {
		r := Request{Path: p}
		status, cold, err := get(p)
		r.Status, r.ColdMs = status, cold
		if err != nil {
			r.Err = err.Error()
			out = append(out, r)
			continue
		}
		if _, warm, err := get(p); err != nil {
			r.Err = err.Error()
		} else {
			r.WarmMs = warm
		}
		out = append(out, r)
	}
	return out, nil
}

func since(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
