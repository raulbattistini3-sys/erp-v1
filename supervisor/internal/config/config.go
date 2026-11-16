// Package config reads a polyvisor application file (YAML, or JSON, which is YAML too).
//
//	application:
//	  name: sample-erp
//	  resources:
//	    memoryBudget: 2048MB      # all services together (a job object / cgroup over all of them)
//	    reserveForSystem: 1024MB  # optional services wait while the machine has less than this free
//	  services:
//	    api:
//	      command: node
//	      args: ["dist/api/index.js"]
//	      required: true
//	      ready: { tcp: 3000 }
//	      memory: { softLimit: 384MB, hardLimit: 512MB }
//	    worker:
//	      command: node
//	      dependsOn: [api]
//	      scheduling: { priority: background, maxConcurrency: 2 }
//
// The memory limits are the whole footprint of the service (its process and everything it starts), not
// a runtime's heap: a JVM's -Xmx or V8's --max-old-space-size stay the service's own business.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type File struct {
	Application Application `yaml:"application" json:"application"`
}

type Application struct {
	Name      string              `yaml:"name" json:"name"`
	Resources Resources           `yaml:"resources" json:"resources"`
	Services  map[string]*Service `yaml:"services" json:"services"`
	// Runtime: the folder of polyvisor's JS runtime (adaptive admission + addon), handed to services as
	// POLYVISOR_RUNTIME. Default: "runtime" next to the executable, when it exists.
	Runtime string `yaml:"runtime" json:"runtime,omitempty"`
	// Events: an NDJSON file the lifecycle events are appended to (optional).
	Events string `yaml:"events" json:"events,omitempty"`
	// Env: variables every service gets (a service's own env wins).
	Env map[string]string `yaml:"env" json:"env,omitempty"`
	// Dir: relative commands, cwds and paths resolve against it; default: the config file's folder.
	Dir string `yaml:"dir" json:"dir,omitempty"`
}

type Resources struct {
	MemoryBudget     Size `yaml:"memoryBudget" json:"memoryBudget"`
	ReserveForSystem Size `yaml:"reserveForSystem" json:"reserveForSystem"`
}

type Service struct {
	Name      string            `yaml:"-" json:"name"`
	Command   string            `yaml:"command" json:"command"`
	Args      []string          `yaml:"args" json:"args,omitempty"`
	Env       map[string]string `yaml:"env" json:"env,omitempty"`
	Cwd       string            `yaml:"cwd" json:"cwd,omitempty"`
	Required  bool              `yaml:"required" json:"required"`
	DependsOn []string          `yaml:"dependsOn" json:"dependsOn,omitempty"`
	Disabled  bool              `yaml:"disabled" json:"disabled,omitempty"`

	Memory     Memory     `yaml:"memory" json:"memory"`
	Scheduling Scheduling `yaml:"scheduling" json:"scheduling"`
	Ready      Ready      `yaml:"ready" json:"ready"`
	Restart    Restart    `yaml:"restart" json:"restart"`
	Stop       Stop       `yaml:"stop" json:"stop"`
}

type Memory struct {
	// SoftLimit: pressure starts here. Windows: a job notification limit (an event, nothing fails);
	// Linux: memory.high (the kernel reclaims and throttles). Services read it as POLYVISOR_MEMORY_SOFT_BYTES.
	SoftLimit Size `yaml:"softLimit" json:"softLimit"`
	// HardLimit: allocations past it fail (Windows: job memory limit) or the OOM killer acts
	// (Linux: memory.max); the service dies and is restarted.
	HardLimit Size `yaml:"hardLimit" json:"hardLimit"`
}

type Scheduling struct {
	// Priority: "normal" (default), "background" (below normal / nice 10) or "idle" (nice 19).
	Priority string `yaml:"priority" json:"priority,omitempty"`
	// MaxConcurrency: how much parallel work the service should take on (POLYVISOR_MAX_CONCURRENCY);
	// polyvisor's adaptive admission starts from it.
	MaxConcurrency int `yaml:"maxConcurrency" json:"maxConcurrency,omitempty"`
}

// Ready: when the service counts as started (its dependents wait for it). None: as soon as it runs.
type Ready struct {
	TCP     string   `yaml:"tcp" json:"tcp,omitempty"`   // "3000" or "host:port"
	HTTP    string   `yaml:"http" json:"http,omitempty"` // a URL answering 2xx/3xx
	Delay   Duration `yaml:"delay" json:"delay,omitempty"`
	Timeout Duration `yaml:"timeout" json:"timeout,omitempty"` // default 120s; past it the start counts as failed
}

type Restart struct {
	// Policy: "always" (default), "on-failure" (not after exit code 0) or "never".
	Policy string `yaml:"policy" json:"policy,omitempty"`
	// MaxRestarts within Window before the service is given up (a required one takes the app down). 0: no cap.
	MaxRestarts int      `yaml:"maxRestarts" json:"maxRestarts,omitempty"`
	Window      Duration `yaml:"window" json:"window,omitempty"`     // default 5m
	Backoff     Duration `yaml:"backoff" json:"backoff,omitempty"`   // first delay, default 1s, doubles
	MaxBackoff  Duration `yaml:"maxBackoff" json:"maxBackoff,omitempty"` // default 30s
}

type Stop struct {
	// Signal: how polyvisor asks the service to stop. "interrupt" (default: Ctrl+C / SIGINT; on Windows the
	// service shares the console's process group, so it gets the console's Ctrl+C itself), "break" (Windows:
	// own process group, CTRL_BREAK; Linux: SIGTERM), "terminate" (SIGTERM / no grace on Windows), "kill".
	Signal string   `yaml:"signal" json:"signal,omitempty"`
	Grace  Duration `yaml:"grace" json:"grace,omitempty"` // default 20s, then the whole process tree is killed
}

// Size: bytes, written as 512MB, 2GB, 1.5GiB, 4096 (bytes)...
type Size uint64

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

func (s Size) MarshalJSON() ([]byte, error) { return []byte(strconv.FormatUint(uint64(s), 10)), nil }
func (s Size) MB() uint64                   { return uint64(s) >> 20 }
func (s Size) String() string {
	switch {
	case s == 0:
		return "0"
	case s%(1<<30) == 0:
		return fmt.Sprintf("%dGB", s>>30)
	case s%(1<<20) == 0:
		return fmt.Sprintf("%dMB", s>>20)
	}
	return fmt.Sprintf("%dB", uint64(s))
}

// ParseSize: binary units (MB = MiB, as -Xmx and the Windows job limits count).
func ParseSize(text string) (Size, error) {
	t := strings.TrimSpace(strings.ToUpper(text))
	if t == "" || t == "0" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   float64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("size %q: want a number with an optional unit (512MB, 2GB)", text)
	}
	return Size(f * mult), nil
}

// Duration: "20s", "1m30s", or a number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if s, err := strconv.ParseFloat(n.Value, 64); err == nil {
		*d = Duration(s * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(time.Duration(d).String())), nil }
func (d Duration) Or(def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return time.Duration(d)
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Application, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	return Parse(data, filepath.Dir(abs))
}

// Parse: dir is where relative paths resolve when the file doesn't say (application.dir).
func Parse(data []byte, dir string) (*Application, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	app := &f.Application
	if app.Dir == "" {
		app.Dir = dir
	} else if !filepath.IsAbs(app.Dir) {
		app.Dir = filepath.Join(dir, app.Dir)
	}
	if app.Name == "" {
		app.Name = "app"
	}
	for name, s := range app.Services {
		if s == nil {
			return nil, fmt.Errorf("service %s: empty", name)
		}
		s.Name = name
	}
	return app, app.Validate()
}

func (app *Application) Validate() error {
	var errs []error
	if len(app.Services) == 0 {
		errs = append(errs, errors.New("no services"))
	}
	for _, s := range app.Services {
		if s.Command == "" {
			errs = append(errs, fmt.Errorf("service %s: no command", s.Name))
		}
		if s.Memory.HardLimit > 0 && s.Memory.SoftLimit > s.Memory.HardLimit {
			errs = append(errs, fmt.Errorf("service %s: softLimit %v over hardLimit %v", s.Name, s.Memory.SoftLimit, s.Memory.HardLimit))
		}
		switch s.Scheduling.Priority {
		case "", "normal", "background", "idle", "high":
		default:
			errs = append(errs, fmt.Errorf("service %s: priority %q (normal, background, idle, high)", s.Name, s.Scheduling.Priority))
		}
		switch s.Restart.Policy {
		case "", "always", "on-failure", "never":
		default:
			errs = append(errs, fmt.Errorf("service %s: restart policy %q (always, on-failure, never)", s.Name, s.Restart.Policy))
		}
		switch s.Stop.Signal {
		case "", "interrupt", "break", "terminate", "kill":
		default:
			errs = append(errs, fmt.Errorf("service %s: stop signal %q (interrupt, break, terminate, kill)", s.Name, s.Stop.Signal))
		}
		for _, d := range s.DependsOn {
			dep, ok := app.Services[d]
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("service %s depends on %s, which isn't defined", s.Name, d))
			case dep.Disabled && !s.Disabled:
				errs = append(errs, fmt.Errorf("service %s depends on %s, which is disabled", s.Name, d))
			}
		}
	}
	if b := app.Resources.MemoryBudget; b > 0 {
		var hard Size
		for _, s := range app.Enabled() {
			hard += s.Memory.HardLimit
			if s.Memory.HardLimit > b {
				errs = append(errs, fmt.Errorf("service %s: hardLimit %v over the memoryBudget %v", s.Name, s.Memory.HardLimit, b))
			}
		}
		_ = hard // over-committing the budget with hard limits is allowed: the budget is the real cap
	}
	if len(errs) == 0 {
		if _, err := app.Order(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Enabled: the services that run, by name.
func (app *Application) Enabled() []*Service {
	var out []*Service
	for _, s := range app.Services {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Order: the enabled services, each after its dependencies (by name within a level); an error names a cycle.
func (app *Application) Order() ([]*Service, error) {
	const (
		unseen = iota
		visiting
		done
	)
	state := map[string]int{}
	var out []*Service
	var visit func(s *Service, path []string) error
	visit = func(s *Service, path []string) error {
		switch state[s.Name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(path, s.Name), " → "))
		}
		state[s.Name] = visiting
		deps := append([]string(nil), s.DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(app.Services[d], append(path, s.Name)); err != nil {
				return err
			}
		}
		state[s.Name] = done
		out = append(out, s)
		return nil
	}
	for _, s := range app.Enabled() {
		if err := visit(s, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Path resolves a path from the config against the application's folder.
func (app *Application) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(app.Dir, p)
}
