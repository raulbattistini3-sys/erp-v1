// Package config loads the cache's settings: defaults, then the JSON file, then the environment.
// An unknown key in the file stops startup (a typo must not silently fall back to a default).
// Credentials never live in the file: it names the environment variables that hold them.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Duration is a time.Duration written as "30s", "5m" in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d Duration) D() time.Duration             { return time.Duration(d) }

type Config struct {
	// Control is the private address of /health, /stats and POST /flush.
	Control   string    `json:"control"`
	Telemetry Telemetry `json:"telemetry"`
	Sites     []Site    `json:"sites"`
}

// Telemetry sends the cache's own numbers to the TCC telemetry server (POST /telemetry/host).
type Telemetry struct {
	URL        string   `json:"url"`        // empty: not sent
	TokenEnv   string   `json:"tokenEnv"`   // the variable holding the bearer token
	Source     string   `json:"source"`     // default "erp-cache"
	InstanceID string   `json:"instanceId"` // default: the host name
	Interval   Duration `json:"interval"`   // default 10s
}

// Site is one upstream (the ERP API, the TCC server) and the routes cached in front of it.
type Site struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	// UpstreamCA is a PEM file the upstream's certificate is checked against, for an https:// upstream
	// signed by a private CA (the launcher's API: the internal CA in data/certs).
	UpstreamCA string `json:"upstreamCa"`
	// Register is the service name this site joins the network registry as, when NETWORK_URL and
	// NETWORK_TOKEN are set (the launcher, `network dev`); the gateway then routes a prefix to it with
	// NETWORK_ROUTES="/api=api-cache,...". Empty: not registered, clients use Listen directly.
	Register string `json:"register"`
	// DatabaseURLEnv names the variable with a postgres:// URL to LISTEN on. It must be a session
	// connection: Supabase's transaction pooler (6543) does not deliver notifications; use 5432.
	// Empty: no feed, entries only expire by TTL (fine for a demo, not for data that must be current).
	DatabaseURLEnv string `json:"databaseUrlEnv"`
	// DatabaseCACertEnv names the variable with the path of the database's CA file (TLS verify-full).
	DatabaseCACertEnv string   `json:"databaseCaCertEnv"`
	Channel           string   `json:"channel"`       // the NOTIFY channel; default "<name>_cache"
	MaxEntries        int      `json:"maxEntries"`    // default 10000
	MaxBytes          int64    `json:"maxBytes"`      // default 64 MiB
	MaxEntryBytes     int      `json:"maxEntryBytes"` // default 2 MiB
	DefaultTTL        Duration `json:"defaultTtl"`    // default 30s
	// Vary are request headers that are part of every key (besides Origin and Accept-Encoding).
	// The ERP answers per user: "authorization" must be here for it.
	Vary []string `json:"vary"`
	// FlushOn are tables whose change drops everything (users, permissions: who may see what).
	FlushOn []string `json:"flushOn"`
	Routes  []Route  `json:"routes"`
	// Dedupe are POST paths whose resends the cache answers itself (see Dedupe).
	Dedupe []Dedupe `json:"dedupe"`
	// Warmup are GET paths requested twice straight at the upstream once it answers, at startup: the
	// first answer pays the upstream's cold start (compilation, JIT, lazy pools), the second does not.
	// The difference is reported (type "cold-start"); nothing is cached.
	Warmup []string `json:"warmup"`
	// WarmupAuthEnv names the variable with an Authorization header value for the warm-up requests
	// (e.g. "Bearer <jwt>"); without it, protected paths answer 401, which still warms the routing.
	WarmupAuthEnv string `json:"warmupAuthEnv"`
	// WarmupWait is how long to wait for the upstream to answer at all; default 2m
	WarmupWait Duration `json:"warmupWait"`
}

// Route is a GET path that may be answered from the cache. ":name" matches one segment, a final
// "*" the rest of the path.
type Route struct {
	Path   string   `json:"path"`
	TTL    Duration `json:"ttl"`    // default: the site's defaultTtl
	Tables []string `json:"tables"` // what the response is read from; a change in any of them drops it
}

// Dedupe is a POST path that writes records identified by fields of its JSON body (telemetry
// envelopes: source, instanceId, bootId, sequence). Once the upstream accepted a record (2xx), a
// resend of it, from a sender that retried after a timeout or after a restart, is answered by the
// cache with that same status and never reaches the upstream: no request, no INSERT ... ON CONFLICT.
// A record the upstream accepts drops the cached GETs that read Invalidates, so a dashboard reading
// through the cache sees it without waiting for the TTL or for the database feed.
type Dedupe struct {
	Path        string   `json:"path"`
	Keys        []string `json:"keys"`        // top-level body fields, all required, that identify a record
	Invalidates []string `json:"invalidates"` // tables the record is written to
	MaxKeys     int      `json:"maxKeys"`     // records remembered, the oldest forgotten first; default 100000
}

var defaults = Config{
	Control:   "127.0.0.1:7071",
	Telemetry: Telemetry{Source: "erp-cache", Interval: Duration(10 * time.Second)},
}

// Load reads path (if it exists or was asked for explicitly) over the defaults, then the environment:
//
//	CACHE_CONTROL_ADDR, TCC_TELEMETRY_URL,
//	CACHE_<SITE>_LISTEN, CACHE_<SITE>_UPSTREAM, CACHE_<SITE>_UPSTREAM_CA, CACHE_<SITE>_REGISTER
//	(<SITE> = the name upper-cased, "-" → "_"; CACHE_<SITE>_REGISTER=off leaves the registry alone)
func Load(path string, explicit bool) (*Config, error) {
	c := defaults
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	case explicit || !os.IsNotExist(err):
		return nil, err
	}
	if v := os.Getenv("CACHE_CONTROL_ADDR"); v != "" {
		c.Control = v
	}
	if v := os.Getenv("TCC_TELEMETRY_URL"); v != "" {
		c.Telemetry.URL = v
	}
	for i := range c.Sites {
		s := &c.Sites[i]
		p := "CACHE_" + strings.ToUpper(strings.ReplaceAll(s.Name, "-", "_")) + "_"
		if v := os.Getenv(p + "LISTEN"); v != "" {
			s.Listen = v
		}
		if v := os.Getenv(p + "UPSTREAM"); v != "" {
			s.Upstream = v
		}
		if v := os.Getenv(p + "UPSTREAM_CA"); v != "" {
			s.UpstreamCA = v
		}
		if v := os.Getenv(p + "REGISTER"); v == "off" {
			s.Register = ""
		} else if v != "" {
			s.Register = v
		}
		fill(s)
	}
	return &c, c.validate()
}

func fill(s *Site) {
	if s.Channel == "" {
		s.Channel = strings.ReplaceAll(s.Name, "-", "_") + "_cache"
	}
	if s.MaxEntries == 0 {
		s.MaxEntries = 10_000
	}
	if s.MaxBytes == 0 {
		s.MaxBytes = 64 << 20
	}
	if s.MaxEntryBytes == 0 {
		s.MaxEntryBytes = 2 << 20
	}
	if s.DefaultTTL == 0 {
		s.DefaultTTL = Duration(30 * time.Second)
	}
	if s.WarmupWait == 0 {
		s.WarmupWait = Duration(2 * time.Minute)
	}
	for i := range s.Routes {
		if s.Routes[i].TTL == 0 {
			s.Routes[i].TTL = s.DefaultTTL
		}
	}
	for i := range s.Dedupe {
		if s.Dedupe[i].MaxKeys == 0 {
			s.Dedupe[i].MaxKeys = 100_000
		}
	}
}

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func (c *Config) validate() error {
	if len(c.Sites) == 0 {
		return fmt.Errorf("no sites configured")
	}
	seen := map[string]bool{c.Control: true}
	names := map[string]bool{}
	for _, s := range c.Sites {
		if s.Name == "" || names[s.Name] {
			return fmt.Errorf("site name %q is empty or repeated", s.Name)
		}
		names[s.Name] = true
		if s.Listen == "" || seen[s.Listen] {
			return fmt.Errorf("site %s: listen %q is empty or already used", s.Name, s.Listen)
		}
		seen[s.Listen] = true
		if u, err := url.Parse(s.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("site %s: upstream %q is not an http(s) URL", s.Name, s.Upstream)
		}
		if !ident.MatchString(s.Channel) {
			return fmt.Errorf("site %s: channel %q must be a lower-case SQL identifier", s.Name, s.Channel)
		}
		for _, t := range s.FlushOn {
			if !ident.MatchString(t) {
				return fmt.Errorf("site %s: flushOn table %q is not a plain table name", s.Name, t)
			}
		}
		for _, p := range s.Warmup {
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("site %s: warmup path %q must start with /", s.Name, p)
			}
		}
		for _, r := range s.Routes {
			if !strings.HasPrefix(r.Path, "/") {
				return fmt.Errorf("site %s: route %q must start with /", s.Name, r.Path)
			}
			if r.TTL.D() <= 0 {
				return fmt.Errorf("site %s: route %s has no positive ttl", s.Name, r.Path)
			}
			if len(r.Tables) == 0 && s.DatabaseURLEnv != "" {
				return fmt.Errorf("site %s: route %s lists no tables, so no change could ever drop it", s.Name, r.Path)
			}
			for _, t := range r.Tables {
				if !ident.MatchString(t) {
					return fmt.Errorf("site %s: route %s: table %q is not a plain table name", s.Name, r.Path, t)
				}
			}
		}
		for _, d := range s.Dedupe {
			if !strings.HasPrefix(d.Path, "/") || len(d.Keys) == 0 {
				return fmt.Errorf("site %s: dedupe %q needs a path starting with / and at least one key", s.Name, d.Path)
			}
			for _, t := range d.Invalidates {
				if !ident.MatchString(t) {
					return fmt.Errorf("site %s: dedupe %s: table %q is not a plain table name", s.Name, d.Path, t)
				}
			}
		}
	}
	return nil
}

// Tables are every table a site's entries depend on (routes and flushOn), each once.
func (s *Site) Tables() []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, r := range s.Routes {
		for _, t := range r.Tables {
			add(t)
		}
	}
	for _, t := range s.FlushOn {
		add(t)
	}
	return out
}
