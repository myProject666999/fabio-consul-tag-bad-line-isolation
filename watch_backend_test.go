package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/fabiolb/fabio/config"
	"github.com/fabiolb/fabio/registry"
	"github.com/fabiolb/fabio/route"
)

// fakeBackend is a registry.Backend whose watches are channels owned by the
// test. Register calls are recorded so that alias handling can be checked.
type fakeBackend struct {
	svc chan string
	man chan string

	mu        sync.Mutex
	registers [][]string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		svc: make(chan string),
		man: make(chan string),
	}
}

func (b *fakeBackend) Register(services []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registers = append(b.registers, append([]string(nil), services...))
	return nil
}

func (b *fakeBackend) Deregister(string) error                          { return nil }
func (b *fakeBackend) DeregisterAll() error                             { return nil }
func (b *fakeBackend) ManualPaths() ([]string, error)                   { return nil, nil }
func (b *fakeBackend) ReadManual(string) (string, uint64, error)        { return "", 0, nil }
func (b *fakeBackend) WriteManual(string, string, uint64) (bool, error) { return false, nil }

func (b *fakeBackend) WatchServices() chan string    { return b.svc }
func (b *fakeBackend) WatchManual() chan string      { return b.man }
func (b *fakeBackend) WatchNoRouteHTML() chan string { return make(chan string, 1) }

func (b *fakeBackend) registerCalls() [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]string(nil), b.registers...)
}

// watchBackendDriver drives watchBackend through its channels. An unbuffered
// send returns as soon as the loop receives the value. A second send of the
// service configuration afterwards is a fence: it can only be received once
// the previous iteration has been fully processed and the loop is back in
// its select, so the test can read the routing table without sleeping.
type watchBackendDriver struct {
	t       *testing.T
	backend *fakeBackend
	svccfg  string
}

func newWatchBackendDriver(t *testing.T) *watchBackendDriver {
	t.Helper()

	b := newFakeBackend()
	prev := registry.Default
	registry.Default = b
	t.Cleanup(func() { registry.Default = prev })

	cfg := &config.Config{}
	cfg.Registry.Backend = "consul"
	cfg.Registry.Consul.KVPath = "/fabio/config"
	cfg.Log.RoutesFormat = "delta"

	go watchBackend(cfg, make(chan bool, 1))

	return &watchBackendDriver{t: t, backend: b}
}

// sendService sends the service configuration and fences with the same
// content.
func (d *watchBackendDriver) sendService(cfg string) {
	d.t.Helper()
	d.svccfg = cfg
	d.backend.svc <- cfg
	d.backend.svc <- cfg
}

// sendManual sends a manual configuration and fences on the service channel
// with the current service configuration, which is a no-op for the loop.
func (d *watchBackendDriver) sendManual(cfg string) {
	d.t.Helper()
	d.backend.man <- cfg
	d.backend.svc <- d.svccfg
}

func (d *watchBackendDriver) table() route.Table {
	d.t.Helper()
	return route.GetTable()
}

// targetURLs returns the target URLs of the route with the given path,
// independent of the host the route was registered under.
func targetURLs(t route.Table, path string) (urls []string) {
	for _, routes := range t {
		for _, r := range routes {
			if r.Path != path {
				continue
			}
			for _, tg := range r.Targets {
				urls = append(urls, tg.URL.String())
			}
		}
	}
	return urls
}

func assertTargets(t *testing.T, step, path string, want ...string) {
	t.Helper()
	got := targetURLs(route.GetTable(), path)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: targets for %s:\ngot  %v\nwant %v", step, path, got, want)
	}
}

// logRecorder collects log lines written during the test.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func captureMainLog(t *testing.T) *logRecorder {
	t.Helper()
	r := &logRecorder{}
	log.SetOutput(recorderWriter{r})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return r
}

type recorderWriter struct{ r *logRecorder }

func (w recorderWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.r.lines = append(w.r.lines, l)
	}
	return len(p), nil
}

func (r *logRecorder) filter(substr string) (out []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// TestWatchBackendIsolatesBadManualConfig walks the acceptance sequence over
// the svc and man channels:
//
//  1. service side: two web instances and an api instance. The bad api tag
//     "urlprefix-/api weight=abc" has already been dropped by the consul
//     layer, so /web has two targets, /api/v2 points at api and /api does not
//     exist.
//  2. manual side: delete the second web instance.
//  3. manual side: append a line with an invalid glob path. The whole manual
//     configuration is rejected, the table of step 2 is kept and the error
//     points at the KV key and line 2.
//  4. service side: the first web instance becomes unhealthy. The last
//     accepted manual configuration is still used, so the /web route loses
//     its remaining target and disappears while /api/v2 stays.
//  5. manual side: the key is restored. The table does not change, the
//     rejection is cleared with one INFO, and sending the same content again
//     produces no new log line.
func TestWatchBackendIsolatesBadManualConfig(t *testing.T) {
	d := newWatchBackendDriver(t)
	logs := captureMainLog(t)

	const (
		svc1 = "route add web /web http://10.0.0.2:8080/\n" +
			"route add web /web http://10.0.0.1:8080/\n" +
			`route add api /api/v2 http://10.0.1.1:9090/ opts "register=api-alias"`

		// service side after 10.0.0.1 became unhealthy
		svc2 = "route add web /web http://10.0.0.2:8080/\n" +
			`route add api /api/v2 http://10.0.1.1:9090/ opts "register=api-alias"`

		man2 = "# --- fabio/config\nroute del web /web http://10.0.0.2:8080/"

		man3 = "# --- fabio/config\n" +
			"route del web /web http://10.0.0.2:8080/\n" +
			"route add x /[x http://10.9.9.9:80/"
	)

	// step 1
	d.sendService(svc1)
	assertTargets(t, "step 1", "/web", "http://10.0.0.2:8080/", "http://10.0.0.1:8080/")
	assertTargets(t, "step 1", "/api/v2", "http://10.0.1.1:9090/")
	assertTargets(t, "step 1", "/api")

	if got := d.backend.registerCalls(); len(got) != 1 || strings.Join(got[0], ",") != "api-alias" {
		t.Errorf("step 1: Register calls = %v, want one call with [api-alias]", got)
	}

	// step 2
	d.sendManual(man2)
	assertTargets(t, "step 2", "/web", "http://10.0.0.1:8080/")

	// step 3: rejected, table unchanged, error points at the key and line.
	d.sendManual(man3)
	assertTargets(t, "step 3", "/web", "http://10.0.0.1:8080/")
	assertTargets(t, "step 3", "/api/v2", "http://10.0.1.1:9090/")

	rejected := logs.filter("invalid manual configuration")
	if len(rejected) != 1 {
		t.Fatalf("step 3: got %d manual rejection lines, want 1:\n%v", len(rejected), rejected)
	}
	if want := `key "fabio/config" line 2`; !strings.Contains(rejected[0], want) {
		t.Errorf("step 3: rejection does not name %s: %s", want, rejected[0])
	}
	if strings.Contains(rejected[0], "line 1") {
		t.Errorf("step 3: rejection points at the wrong line: %s", rejected[0])
	}

	// No Register call with an empty alias list: that would deregister all
	// aliases of the still effective configuration.
	for _, call := range d.backend.registerCalls()[1:] {
		if len(call) == 0 {
			t.Errorf("step 3: Register was called with no aliases: %v", call)
		}
	}

	// step 4: service side change while the manual configuration is rejected.
	d.sendService(svc2)
	assertTargets(t, "step 4", "/web")
	assertTargets(t, "step 4", "/api/v2", "http://10.0.1.1:9090/")

	// step 5: the key is restored; the rejection state clears once.
	d.sendManual(man2)
	assertTargets(t, "step 5", "/api/v2", "http://10.0.1.1:9090/")

	if got := logs.filter("Manual configuration accepted again"); len(got) != 1 {
		t.Errorf("step 5: got %d acceptance lines, want 1:\n%v", len(got), got)
	}

	// sending the same content again produces no new log line
	d.sendManual(man2)
	assertTargets(t, "step 5b", "/api/v2", "http://10.0.1.1:9090/")
	if got := logs.filter("invalid manual configuration"); len(got) != 1 {
		t.Errorf("step 5b: manual rejection logged again: %v", got)
	}
	if got := logs.filter("Manual configuration accepted again"); len(got) != 1 {
		t.Errorf("step 5b: acceptance logged again: %v", got)
	}
}
