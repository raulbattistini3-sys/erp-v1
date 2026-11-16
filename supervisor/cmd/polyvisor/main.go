// polyvisor: a supervisor for local applications made of several runtimes (Node, Bun, Java, Go, any
// executable), each service in a memory-limited container of its own (Windows job objects, Linux cgroups
// v2) inside one budget for the whole application.
//
//	polyvisor run [-c polyvisor.yaml] [--events FILE] [--control ADDR]
//	polyvisor validate [-c polyvisor.yaml]
//	polyvisor status [--app NAME] [--json]
//	polyvisor events [--app NAME] [--follow] [--since N]
//	polyvisor start|stop|restart SERVICE [--app NAME]
//	polyvisor shutdown [--app NAME]
//	polyvisor version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/config"
	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/control"
	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/supervisor"
)

var version = "0.1.0" // -ldflags "-X main.version=..."

const usage = `polyvisor — a resource-aware supervisor for multi-runtime local applications

usage:
  polyvisor run       [-c FILE] [--events FILE] [--control ADDR]   start the application (foreground)
  polyvisor validate  [-c FILE]                                    check a config and print the start order
  polyvisor status    [--app NAME] [--json]                        services, states, memory
  polyvisor events    [--app NAME] [--follow] [--since N]          lifecycle and resource events
  polyvisor start|stop|restart SERVICE [--app NAME]
  polyvisor shutdown  [--app NAME]
  polyvisor version

FILE defaults to polyvisor.yaml (or .yml / .json) in the current folder.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "run":
		code, err = run(args)
	case "validate":
		err = validate(args)
	case "status":
		err = status(args)
	case "events":
		err = events(args)
	case "start", "stop", "restart":
		err = act(cmd, args)
	case "shutdown":
		err = shutdown(args)
	case "version", "--version", "-v":
		fmt.Println("polyvisor", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "polyvisor:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func configFlag(fs *flag.FlagSet) *string {
	c := fs.String("c", "", "config file")
	fs.StringVar(c, "config", "", "config file")
	return c
}

func findConfig(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	for _, name := range []string{"polyvisor.yaml", "polyvisor.yml", "polyvisor.json"} {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("no polyvisor.yaml here; pass -c FILE")
}

func load(path string) (*config.Application, string, error) {
	path, err := findConfig(path)
	if err != nil {
		return nil, "", err
	}
	app, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	abs, _ := filepath.Abs(path)
	return app, abs, nil
}

func run(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg := configFlag(fs)
	eventsFile := fs.String("events", "", "append events to this NDJSON file (overrides application.events)")
	addr := fs.String("control", "127.0.0.1:0", "control API address")
	_ = fs.Parse(args)
	app, path, err := load(*cfg)
	if err != nil {
		return 1, err
	}
	if *eventsFile != "" {
		app.Events = *eventsFile
	}
	if c, err := control.Find(app.Name); err == nil && alive(c) {
		return 1, fmt.Errorf("%s is already running (polyvisor pid %d at %s)", app.Name, c.State.PID, c.State.URL)
	}
	srv, err := control.Listen(*addr)
	if err != nil {
		return 1, err
	}
	defer srv.Close()
	sup, err := supervisor.New(app, supervisor.Options{ControlURL: srv.URL, Token: srv.Token})
	if err != nil {
		return 1, err
	}
	unpublish, err := srv.Publish(app.Name, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "polyvisor: no state file (status/stop won't find this run):", err)
	}
	defer unpublish()
	go srv.Serve(sup)

	// Ctrl+C / console close / SIGTERM: a graceful shutdown; a second one kills everything
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		first := <-sigs
		sup.Shutdown(first == os.Interrupt)
		<-sigs
		sup.Kill()
	}()
	return sup.Run(), nil
}

func alive(c *control.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.Do(ctx, "GET", "/v1/status", nil) == nil
}

func validate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfg := configFlag(fs)
	_ = fs.Parse(args)
	app, path, err := load(*cfg)
	if err != nil {
		return err
	}
	order, _ := app.Order()
	fmt.Printf("%s: %s, budget %v, reserve %v\n", path, app.Name, app.Resources.MemoryBudget, app.Resources.ReserveForSystem)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tSERVICE\tREQUIRED\tSOFT\tHARD\tPRIORITY\tDEPENDS ON")
	for i, s := range order {
		fmt.Fprintf(w, "%d\t%s\t%v\t%v\t%v\t%s\t%s\n", i+1, s.Name, s.Required, s.Memory.SoftLimit, s.Memory.HardLimit,
			def(s.Scheduling.Priority, "normal"), strings.Join(s.DependsOn, ", "))
	}
	return w.Flush()
}

func appFlag(fs *flag.FlagSet) *string { return fs.String("app", "", "application name (when several run)") }

func client(app string) (*control.Client, error) { return control.Find(app) }

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	app := appFlag(fs)
	asJSON := fs.Bool("json", false, "raw JSON")
	_ = fs.Parse(args)
	c, err := client(*app)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.Do(context.Background(), "GET", "/v1/status", os.Stdout)
	}
	var st supervisor.Status
	if err := c.Do(context.Background(), "GET", "/v1/status", &st); err != nil {
		return err
	}
	fmt.Printf("%s  polyvisor pid %d, up %v", st.App, st.PID, time.Since(st.StartedAt).Round(time.Second))
	if st.Stopping {
		fmt.Print("  (stopping)")
	}
	fmt.Printf("\nmemory: services %s", mb(st.Usage.MemoryBytes))
	if st.Budget > 0 {
		fmt.Printf(" of %s budget", mb(st.Budget))
	}
	if st.System.TotalBytes > 0 {
		fmt.Printf("; machine %s free of %s (%d%% load)", mb(st.System.AvailableBytes), mb(st.System.TotalBytes), st.System.LoadPercent)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSTATE\tPID\tMEMORY\tSOFT/HARD\tPRESSURE\tRESTARTS\tPROCS\tDETAIL")
	for _, s := range st.Services {
		pid := "-"
		if s.PID != 0 {
			pid = fmt.Sprint(s.PID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s/%s\t%s\t%d\t%d\t%s\n", s.Name, s.State, pid, mb(s.Usage.MemoryBytes),
			mb(s.SoftBytes), mb(s.HardBytes), s.Pressure, s.Restarts, s.Usage.Processes, s.Detail)
	}
	return w.Flush()
}

func events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	app := appFlag(fs)
	follow := fs.Bool("follow", false, "keep streaming")
	since := fs.Uint64("since", 0, "only events after this sequence number")
	_ = fs.Parse(args)
	c, err := client(*app)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/v1/events?since=%d", *since)
	if *follow {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		err := c.Do(ctx, "GET", path+"&follow=1", os.Stdout)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var list []supervisor.Event
	if err := c.Do(context.Background(), "GET", path, &list); err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	for _, e := range list {
		line, _ := json.Marshal(e)
		fmt.Println(string(line))
	}
	return nil
}

func act(action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	app := appFlag(fs)
	_ = fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: polyvisor %s SERVICE [--app NAME]", action)
	}
	c, err := client(*app)
	if err != nil {
		return err
	}
	if err := c.Do(context.Background(), "POST", "/v1/services/"+fs.Arg(0)+"/"+action, nil); err != nil {
		return err
	}
	fmt.Printf("%s: %s requested\n", fs.Arg(0), action)
	return nil
}

// reorder: flags after the service name too (polyvisor restart api --app x)
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			rest = append(rest, args[i])
		}
	}
	return append(flags, rest...)
}

func shutdown(args []string) error {
	fs := flag.NewFlagSet("shutdown", flag.ExitOnError)
	app := appFlag(fs)
	_ = fs.Parse(args)
	c, err := client(*app)
	if err != nil {
		return err
	}
	if err := c.Do(context.Background(), "POST", "/v1/shutdown", nil); err != nil {
		return err
	}
	fmt.Printf("%s: shutting down\n", c.State.App)
	return nil
}

func mb(b uint64) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0fMB", float64(b)/(1<<20))
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
