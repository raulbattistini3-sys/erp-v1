// Package network joins a cache site to the ERP's network registry (sample-ERP/network), so the
// gateway can route a path prefix to it: NETWORK_ROUTES="/api=api-cache,..." sends /api through the
// cache, which forwards what it does not answer to the API.
//
// It is the small part of network/client the cache needs (register, heartbeat, leave), kept here
// because the cache is its own module at the root, beside the ERP and the TCC, not inside either.
// Every call is signed like network/registry/sign.go:
//
//	X-Network-Signature: t=<unix s>,n=<nonce>,s=hex(HMAC-SHA256(token, t\nn\nMETHOD\npath\nhex(sha256(body))))
package network

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const header = "X-Network-Signature"

// Registration is what the registry stores for an instance (network/registry.Registration).
type Registration struct {
	Address    string            `json:"address"`
	Version    string            `json:"version,omitempty"`
	Health     string            `json:"health,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
	TTLSeconds int               `json:"ttlSeconds,omitempty"`
}

type Client struct {
	base  string
	token []byte
	http  *http.Client
}

// FromEnv reads NETWORK_URL and NETWORK_TOKEN; false when either is missing.
func FromEnv() (*Client, bool) {
	base, token := strings.TrimSpace(os.Getenv("NETWORK_URL")), os.Getenv("NETWORK_TOKEN")
	if base == "" || token == "" {
		return nil, false
	}
	return &Client{base: strings.TrimRight(base, "/"), token: []byte(token), http: &http.Client{Timeout: 5 * time.Second}}, true
}

// Sign returns the signature header value for one request.
func Sign(token []byte, method, path string, body []byte, now time.Time) string {
	var n [12]byte
	_, _ = rand.Read(n[:])
	nonce := hex.EncodeToString(n[:])
	t := strconv.FormatInt(now.Unix(), 10)
	sum := sha256.Sum256(body)
	h := hmac.New(sha256.New, token)
	fmt.Fprintf(h, "%s\n%s\n%s\n%s\n%s", t, nonce, strings.ToUpper(method), path, hex.EncodeToString(sum[:]))
	return fmt.Sprintf("t=%s,n=%s,s=%s", t, nonce, hex.EncodeToString(h.Sum(nil)))
}

func (c *Client) do(ctx context.Context, method, path string, in any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, Sign(c.token, method, path, body, time.Now()))
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Keep registers service/id now and every `every` until ctx ends, then leaves. A failed heartbeat is
// logged once per streak and retried on the next tick (the network may start after the cache).
func (c *Client) Keep(ctx context.Context, service, id string, reg Registration, every time.Duration, log *slog.Logger) {
	if reg.TTLSeconds == 0 {
		reg.TTLSeconds = int(3 * every / time.Second)
	}
	path := "/registry/" + service + "/" + id
	failing := false
	beat := func() {
		if err := c.do(ctx, http.MethodPut, path, reg); err != nil {
			if !failing && ctx.Err() == nil {
				log.Warn("network registration failed; retrying", "service", service, "err", err)
			}
			failing = true
			return
		}
		if failing {
			log.Info("registered in the network again", "service", service)
		} else {
			log.Info("registered in the network", "service", service, "id", id, "address", reg.Address)
		}
		failing = false
	}
	beat()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			lctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = c.do(lctx, http.MethodDelete, path, nil)
			cancel()
			return
		case <-t.C:
			beat()
		}
	}
}

// InstanceID is service-host-random, like network/client.NewID.
func InstanceID(service string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, host)
	if len(host) > 24 {
		host = host[:24]
	}
	return fmt.Sprintf("%s-%s-%s", service, host, hex.EncodeToString(b[:]))
}
