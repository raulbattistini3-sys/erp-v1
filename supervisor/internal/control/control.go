// Package control is polyvisor's local HTTP API (127.0.0.1, a bearer token) and the state file the CLI finds
// a running supervisor by.
//
//	GET  /v1/status                      the application and every service
//	GET  /v1/services/{name}             one service (its pressure, for the JS runtime's admission)
//	GET  /v1/events?since=N[&follow=1]   lifecycle events; follow streams NDJSON
//	POST /v1/services/{name}/{start|stop|restart}
//	POST /v1/shutdown
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/supervisor"
)

// State: what the state file holds, for the CLI.
type State struct {
	App     string    `json:"app"`
	PID     int       `json:"pid"`
	URL     string    `json:"url"`
	Token   string    `json:"token"`
	Config  string    `json:"config"`
	Started time.Time `json:"started"`
}

// Home: POLYVISOR_HOME, or %LOCALAPPDATA%\polyvisor, or ~/.polyvisor.
func Home() string {
	if h := os.Getenv("POLYVISOR_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "polyvisor")
		}
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".polyvisor")
	}
	return filepath.Join(os.TempDir(), "polyvisor")
}

func statePath(app string) string { return filepath.Join(Home(), app+".json") }

// Server: the listener is bound by Listen (the URL goes to the services' env before they start), Serve
// answers once the supervisor exists.
type Server struct {
	ln    net.Listener
	URL   string
	Token string
	state string
}

func Listen(addr string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("control listener: %w", err)
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return &Server{ln: ln, URL: "http://" + ln.Addr().String(), Token: hex.EncodeToString(b)}, nil
}

// Publish writes the state file; the returned func removes it.
func (s *Server) Publish(app, config string) (func(), error) {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return func() {}, err
	}
	s.state = statePath(app)
	data, _ := json.MarshalIndent(State{App: app, PID: os.Getpid(), URL: s.URL, Token: s.Token, Config: config, Started: time.Now()}, "", "  ")
	if err := os.WriteFile(s.state, data, 0o600); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Remove(s.state) }, nil
}

func (s *Server) Serve(sup *supervisor.Supervisor) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { reply(w, sup.Status()) })
	mux.HandleFunc("GET /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		for _, st := range sup.Status().Services {
			if st.Name == r.PathValue("name") {
				reply(w, st)
				return
			}
		}
		http.Error(w, "no such service", http.StatusNotFound)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		if r.URL.Query().Get("follow") == "" {
			reply(w, sup.Events(since))
			return
		}
		ch, stop := sup.Follow()
		defer stop()
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		last := since
		for _, e := range sup.Events(since) {
			_ = enc.Encode(e)
			last = e.Seq
		}
		flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				if e.Seq <= last {
					continue
				}
				if enc.Encode(e) != nil {
					return
				}
				last = e.Seq
				flush()
			}
		}
	})
	mux.HandleFunc("POST /v1/services/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := sup.Control(r.PathValue("name"), r.PathValue("action")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		reply(w, map[string]string{"ok": r.PathValue("action")})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]string{"ok": "shutdown"})
		go sup.Shutdown(false)
	})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+s.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	_ = srv.Serve(s.ln)
}

func (s *Server) Close() { _ = s.ln.Close() }

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Client talks to a running supervisor, found by its state file.
type Client struct {
	State State
	http  *http.Client
}

// Find: the supervisor of app, or the only one running when app is "".
func Find(app string) (*Client, error) {
	var path string
	if app != "" {
		path = statePath(app)
	} else {
		matches, _ := filepath.Glob(filepath.Join(Home(), "*.json"))
		switch len(matches) {
		case 0:
			return nil, errors.New("no polyvisor is running (no state file in " + Home() + ")")
		case 1:
			path = matches[0]
		default:
			var names []string
			for _, m := range matches {
				names = append(names, strings.TrimSuffix(filepath.Base(m), ".json"))
			}
			return nil, fmt.Errorf("several applications are running (%s): pass --app", strings.Join(names, ", "))
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no polyvisor is running for %q: %w", app, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &Client{State: st, http: &http.Client{}}, nil
}

func (c *Client) Do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.State.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.State.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("polyvisor (pid %d) does not answer at %s: %w", c.State.PID, c.State.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
