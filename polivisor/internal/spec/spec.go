// Package spec describes the services the polivisor runs and how they relate.
package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Duration accepts "5s"-style strings (or seconds as numbers) in JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Ready struct {
	TCP     string   `json:"tcp,omitempty"` // host:port that must accept connections
	Timeout Duration `json:"timeout,omitempty"`
}

type Restart struct {
	Max        int      `json:"max"` // restarts allowed inside Window before giving up
	Window     Duration `json:"window"`
	BackoffMin Duration `json:"backoff_min"`
	BackoffMax Duration `json:"backoff_max"`
}

// Runtime lets the polivisor inject heap flags for the service's runtime.
// node: --max-old-space-size, java: -Xmx (+metaspace/direct caps). bun/go: none
// (Bun is JavaScriptCore and ignores V8 flags; the job memory cap is the limit).
type Runtime struct {
	Kind   string `json:"kind,omitempty"` // node | java | bun | go | native
	HeapMB int    `json:"heap_mb,omitempty"`
}

type Service struct {
	Name      string            `json:"name"`
	Program   string            `json:"program"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	DependsOn []string          `json:"depends_on,omitempty"` // must be ready first; failure blocks
	After     []string          `json:"after,omitempty"`      // ordering only; failure tolerated
	Ready     Ready             `json:"ready,omitempty"`
	Critical  bool              `json:"critical,omitempty"` // giving up stops the whole stack
	Optional  bool              `json:"optional,omitempty"` // missing program => skipped
	Restart   Restart           `json:"restart,omitempty"`
	MemoryMB  int               `json:"memory_mb,omitempty"`        // hard Job Object cap
	MinFreeMB int               `json:"min_available_mb,omitempty"` // admission: wait for free RAM
	Runtime   Runtime           `json:"runtime,omitempty"`
}

type Config struct {
	Control  string    `json:"control,omitempty"` // 127.0.0.1:port for /status
	Grace    Duration  `json:"grace,omitempty"`
	Services []Service `json:"services"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	if _, err := c.Tiers(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Defaults() {
	if c.Grace.Duration == 0 {
		c.Grace.Duration = 20 * time.Second
	}
	for i := range c.Services {
		s := &c.Services[i]
		if s.Ready.Timeout.Duration == 0 {
			s.Ready.Timeout.Duration = 60 * time.Second
		}
		r := &s.Restart
		if r.Max == 0 {
			r.Max = 5
		}
		if r.Window.Duration == 0 {
			r.Window.Duration = 5 * time.Minute
		}
		if r.BackoffMin.Duration == 0 {
			r.BackoffMin.Duration = 2 * time.Second
		}
		if r.BackoffMax.Duration == 0 {
			r.BackoffMax.Duration = 30 * time.Second
		}
	}
}

func (c *Config) Find(name string) *Service {
	for i := range c.Services {
		if c.Services[i].Name == name {
			return &c.Services[i]
		}
	}
	return nil
}

// Tiers groups services into start waves from depends_on + after.
func (c *Config) Tiers() ([][]string, error) {
	idx := map[string]*Service{}
	for i := range c.Services {
		s := &c.Services[i]
		if s.Name == "" || s.Program == "" {
			return nil, fmt.Errorf("service #%d needs name and program", i)
		}
		if idx[s.Name] != nil {
			return nil, fmt.Errorf("duplicate service %q", s.Name)
		}
		idx[s.Name] = s
	}
	level := map[string]int{}
	var visit func(string, map[string]bool) (int, error)
	visit = func(n string, stack map[string]bool) (int, error) {
		if l, ok := level[n]; ok {
			return l, nil
		}
		if stack[n] {
			return 0, fmt.Errorf("dependency cycle through %q", n)
		}
		stack[n] = true
		s := idx[n]
		l := 0
		for _, d := range append(append([]string{}, s.DependsOn...), s.After...) {
			if idx[d] == nil {
				return 0, fmt.Errorf("%s depends on unknown %q", n, d)
			}
			dl, err := visit(d, stack)
			if err != nil {
				return 0, err
			}
			if dl+1 > l {
				l = dl + 1
			}
		}
		delete(stack, n)
		level[n] = l
		return l, nil
	}
	var tiers [][]string
	for _, s := range c.Services {
		l, err := visit(s.Name, map[string]bool{})
		if err != nil {
			return nil, err
		}
		for len(tiers) <= l {
			tiers = append(tiers, nil)
		}
		tiers[l] = append(tiers[l], s.Name)
	}
	return tiers, nil
}

// CommandLine returns args with runtime heap flags prepended.
func (s *Service) CommandLine() []string {
	h := s.Runtime.HeapMB
	var pre []string
	switch s.Runtime.Kind {
	case "node":
		if h > 0 {
			pre = []string{"--max-old-space-size=" + strconv.Itoa(h)}
		}
	case "java":
		if h > 0 {
			pre = []string{"-Xms128m", "-Xmx" + strconv.Itoa(h) + "m",
				"-XX:MaxMetaspaceSize=192m", "-XX:MaxDirectMemorySize=128m"}
		}
	}
	return append(pre, s.Args...)
}
