// Package feed keeps a site's entries current: it LISTENs on the site's channel, where the triggers
// from Triggers() send the name of every table a statement changed.
//
// The connection is the cache's only proof that nothing changed. When it drops (or a ping fails), the
// site is told the feed is down (it forgets everything and bypasses) before anything else happens, and
// only after LISTEN succeeds again is it told the feed is up.
package feed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Target is what the feed reports to (proxy.Site).
type Target interface {
	SetFeed(up bool)
	Changed(table string)
}

// Feed follows one channel.
type Feed struct {
	URL       string // postgres:// session connection (not a transaction pooler)
	CACert    string // path of the CA file; empty: the URL's sslmode / sslrootcert decide
	Channel   string
	Target    Target
	Log       *slog.Logger
	OnState   func(up bool, err error) // optional, e.g. lifecycle telemetry
	PingEvery time.Duration            // default 15s
}

// Run follows the channel until ctx ends, reconnecting with backoff (0.5 s doubling to 30 s, jittered).
func (f *Feed) Run(ctx context.Context) {
	if f.PingEvery == 0 {
		f.PingEvery = 15 * time.Second
	}
	backoff := 500 * time.Millisecond
	wasUp := false
	for ctx.Err() == nil {
		err := f.once(ctx, func() {
			wasUp = true
			backoff = 500 * time.Millisecond
		})
		if ctx.Err() != nil {
			return
		}
		f.Target.SetFeed(false)
		if wasUp {
			f.Log.Warn("feed down: forgetting everything until it is back", "channel", f.Channel, "err", err)
			if f.OnState != nil {
				f.OnState(false, err)
			}
			wasUp = false
		} else {
			f.Log.Warn("feed: cannot connect", "channel", f.Channel, "err", err, "retryIn", backoff)
		}
		sleep := backoff/2 + time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// ConnConfig parses a postgres:// URL and, when caCert names a PEM file, verifies the server against it.
func ConnConfig(url, caCert string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	if caCert != "" {
		pem, err := os.ReadFile(caCert)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", caCert)
		}
		cfg.TLSConfig = &tls.Config{RootCAs: pool, ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		cfg.Fallbacks = nil
	}
	return cfg, nil
}

func (f *Feed) once(ctx context.Context, connected func()) error {
	cfg, err := ConnConfig(f.URL, f.CACert)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := pgx.ConnectConfig(cctx, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if strings.Contains(cfg.Host, "pooler.supabase.com") && cfg.Port == 6543 {
		return errors.New("port 6543 is Supabase's transaction pooler, which does not deliver LISTEN/NOTIFY: use the session pooler or direct connection (5432)")
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{f.Channel}.Sanitize()); err != nil {
		return err
	}
	connected()
	f.Target.SetFeed(true)
	f.Log.Info("feed up", "channel", f.Channel)
	if f.OnState != nil {
		f.OnState(true, nil)
	}
	for {
		wctx, cancel := context.WithTimeout(ctx, f.PingEvery)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			f.Target.Changed(n.Payload)
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			// quiet for a while: make sure it is the database that is quiet, not a dead connection
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		default:
			return err
		}
	}
}

// Triggers is the SQL that makes `tables` send their name on `channel` after every statement that
// changes them (one notification per statement; Postgres also folds identical ones in a transaction).
// It needs Postgres 14+ (CREATE OR REPLACE TRIGGER) and may be run again safely.
func Triggers(channel string, tables []string) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE FUNCTION cache_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify(TG_ARGV[0], TG_TABLE_NAME);
  RETURN NULL;
END $$;
`)
	for _, t := range tables {
		id := pgx.Identifier{t}.Sanitize()
		fmt.Fprintf(&b, "CREATE OR REPLACE TRIGGER cache_notify_%s AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH STATEMENT EXECUTE FUNCTION cache_notify('%s');\n", t, id, channel)
		fmt.Fprintf(&b, "CREATE OR REPLACE TRIGGER cache_notify_%s_truncate AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION cache_notify('%s');\n", t, id, channel)
	}
	return b.String()
}
