package config

import (
	"strings"
	"testing"
	"time"
)

const sample = `
application:
  name: sample-erp
  resources:
    memoryBudget: 2048MB
    reserveForSystem: 1GB
  services:
    worker:
      command: node
      args: ["dist/worker/index.js"]
      dependsOn: [api]
      memory: { softLimit: 256MB, hardLimit: 384MB }
      scheduling: { priority: background, maxConcurrency: 2 }
    api:
      command: node
      required: true
      ready: { tcp: 3000, timeout: 30 }
      memory: { softLimit: 384MB, hardLimit: 512MB }
      stop: { grace: 5s }
    billing:
      command: java
      args: ["-Xmx512m", "-jar", "billing.jar"]
      dependsOn: [api]
      memory: { hardLimit: 768MiB }
`

func TestParse(t *testing.T) {
	app, err := Parse([]byte(sample), "/srv/app")
	if err != nil {
		t.Fatal(err)
	}
	if app.Resources.MemoryBudget != 2048<<20 || app.Resources.ReserveForSystem != 1<<30 {
		t.Fatalf("resources %+v", app.Resources)
	}
	api := app.Services["api"]
	if !api.Required || api.Ready.TCP != "3000" || api.Ready.Timeout.Or(0) != 30*time.Second || api.Stop.Grace.Or(0) != 5*time.Second {
		t.Fatalf("api %+v", api)
	}
	if app.Services["billing"].Memory.HardLimit != 768<<20 {
		t.Fatal("768MiB")
	}
	order, _ := app.Order()
	var names []string
	for _, s := range order {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "api,billing,worker" {
		t.Fatalf("order %s", got)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"cycle": `application: {services: {a: {command: x, dependsOn: [b]}, b: {command: x, dependsOn: [a]}}}`,
		"missing dep": `application: {services: {a: {command: x, dependsOn: [nope]}}}`,
		"soft over hard": `application: {services: {a: {command: x, memory: {softLimit: 2GB, hardLimit: 1GB}}}}`,
		"over budget": `application: {resources: {memoryBudget: 1GB}, services: {a: {command: x, memory: {hardLimit: 2GB}}}}`,
		"unknown field": `application: {services: {a: {command: x, memroy: {}}}}`,
		"bad size": `application: {services: {a: {command: x, memory: {hardLimit: lots}}}}`,
		"bad priority": `application: {services: {a: {command: x, scheduling: {priority: urgent}}}}`,
	}
	for name, text := range cases {
		if _, err := Parse([]byte(text), "."); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseSize(t *testing.T) {
	for text, want := range map[string]Size{"512MB": 512 << 20, "2G": 2 << 30, "1.5GiB": 3 << 29, "4096": 4096, " 64 kb ": 64 << 10, "": 0} {
		got, err := ParseSize(text)
		if err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", text, got, err, want)
		}
	}
}
