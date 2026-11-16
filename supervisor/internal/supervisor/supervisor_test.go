package supervisor

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/config"
)

// The test binary doubles as the services: POLYVISOR_TEST_HELPER says what to do.
func TestMain(m *testing.M) {
	switch os.Getenv("POLYVISOR_TEST_HELPER") {
	case "":
		os.Exit(m.Run())
	case "listen":
		ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
		if err != nil {
			os.Exit(5)
		}
		fmt.Println("listening", ln.Addr(), "as", os.Getenv("POLYVISOR_SERVICE"), "hard", os.Getenv("POLYVISOR_MEMORY_HARD_BYTES"))
		for {
			c, err := ln.Accept()
			if err == nil {
				c.Close()
			}
		}
	case "crash":
		fmt.Println("crashing")
		os.Exit(3)
	case "alloc":
		// commits 16 MB at a time until an allocation fails (the job limit) → exit 7
		var keep [][]byte
		defer func() {
			if recover() != nil {
				os.Exit(7)
			}
		}()
		for i := 0; i < 200; i++ {
			b := make([]byte, 16<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			keep = append(keep, b)
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Println("allocated", len(keep)*16, "MB without hitting a limit")
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Hour)
	}
}

type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *buffer) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func start(t *testing.T, yaml string) (*Supervisor, *buffer, chan int) {
	t.Helper()
	exe, _ := os.Executable()
	yaml = strings.ReplaceAll(yaml, "SELF", strings.ReplaceAll(exe, `\`, `/`))
	app, err := config.Parse([]byte(yaml), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := &buffer{}
	s, err := New(app, Options{Out: out, Sample: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- s.Run() }()
	t.Cleanup(func() {
		s.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	return s, out, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(s *Supervisor, name string) string {
	for _, st := range s.Status().Services {
		if st.Name == name {
			return st.State
		}
	}
	return ""
}

func TestDependenciesWaitForReadiness(t *testing.T) {
	port := freePort(t)
	s, out, done := start(t, fmt.Sprintf(`
application:
  name: deps
  services:
    api:
      command: SELF
      required: true
      env: {POLYVISOR_TEST_HELPER: listen, PORT: "%d"}
      ready: {tcp: %d, delay: 300ms}
      memory: {hardLimit: 256MB}
      stop: {signal: kill}
    worker:
      command: SELF
      dependsOn: [api]
      env: {POLYVISOR_TEST_HELPER: sleep}
      stop: {signal: kill}
`, port, port))
	waitFor(t, "both ready", func() bool { return state(s, "api") == Ready && state(s, "worker") == Ready })
	var apiReady, workerStarted uint64
	for _, e := range s.Events(0) {
		if e.Service == "api" && e.Kind == "ready" {
			apiReady = e.Seq
		}
		if e.Service == "worker" && e.Kind == "started" {
			workerStarted = e.Seq
		}
	}
	if apiReady == 0 || workerStarted < apiReady {
		t.Fatalf("worker started (seq %d) before api was ready (seq %d)", workerStarted, apiReady)
	}
	if !strings.Contains(out.String(), "hard 268435456") {
		t.Fatalf("api did not see its limit in POLYVISOR_MEMORY_HARD_BYTES:\n%s", out)
	}
	s.Shutdown(false)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown hung")
	}
	// worker (the dependent) stopped before api
	var workerStopped, apiStopping uint64
	for _, e := range s.Events(0) {
		if e.Service == "worker" && e.Kind == "exited" {
			workerStopped = e.Seq
		}
		if e.Service == "api" && e.Kind == "stopping" {
			apiStopping = e.Seq
		}
	}
	if workerStopped == 0 || apiStopping < workerStopped {
		t.Fatalf("api stopping (seq %d) before worker exited (seq %d)", apiStopping, workerStopped)
	}
}

func TestRequiredServiceGivingUpStopsTheApplication(t *testing.T) {
	_, _, done := start(t, `
application:
  name: crash
  services:
    flaky:
      command: SELF
      required: true
      env: {POLYVISOR_TEST_HELPER: crash}
      restart: {maxRestarts: 2, backoff: 50ms}
    bystander:
      command: SELF
      env: {POLYVISOR_TEST_HELPER: sleep}
      stop: {signal: kill}
`)
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("did not give up")
	}
}

func TestManualStopAndStart(t *testing.T) {
	s, _, _ := start(t, `
application:
  name: manual
  services:
    a:
      command: SELF
      env: {POLYVISOR_TEST_HELPER: sleep}
      stop: {signal: kill}
`)
	waitFor(t, "a ready", func() bool { return state(s, "a") == Ready })
	if err := s.Control("a", "stop"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a stopped", func() bool { return state(s, "a") == Stopped })
	if err := s.Control("a", "start"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a ready again", func() bool { return state(s, "a") == Ready })
	if err := s.Control("nope", "start"); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func TestHardLimitEndsTheService(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cgroups need delegation; covered on Windows by job objects")
	}
	s, _, _ := start(t, `
application:
  name: limits
  services:
    hog:
      command: SELF
      env: {POLYVISOR_TEST_HELPER: alloc}
      memory: {softLimit: 64MB, hardLimit: 128MB}
      restart: {policy: never}
`)
	waitFor(t, "hog to exit", func() bool { return state(s, "hog") == Stopped })
	kinds := map[string]bool{}
	var exit *Exit
	for _, e := range s.Events(0) {
		if e.Service == "hog" {
			kinds[e.Kind] = true
		}
	}
	for _, st := range s.Status().Services {
		exit = st.LastExit
	}
	if !kinds["soft-limit"] || !kinds["hard-limit"] {
		t.Fatalf("events %v", kinds)
	}
	if exit == nil || exit.Code == 0 || exit.Reason != "hard-limit" {
		t.Fatalf("exit %+v", exit)
	}
}

func TestAdmissionDefersOptionalServices(t *testing.T) {
	s, _, _ := start(t, `
application:
  name: admission
  resources: {reserveForSystem: 1000000GB}
  services:
    core:
      command: SELF
      required: true
      env: {POLYVISOR_TEST_HELPER: sleep}
      stop: {signal: kill}
    extra:
      command: SELF
      env: {POLYVISOR_TEST_HELPER: sleep}
      stop: {signal: kill}
`)
	waitFor(t, "core ready, extra deferred", func() bool { return state(s, "core") == Ready && state(s, "extra") == Deferred })
	if err := s.Control("extra", "start"); err != nil { // an explicit start overrides admission
		t.Fatal(err)
	}
	waitFor(t, "extra ready", func() bool { return state(s, "extra") == Ready })
}
