package consul

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/fabiolb/fabio/config"
	"github.com/hashicorp/consul/api"
)

// fakeConsul is a minimal Consul HTTP API substitute. It serves the catalog
// service endpoint from a per-service list of instances which the test can
// switch between.
type fakeConsul struct {
	mu    sync.Mutex
	svcs  map[string][]*api.CatalogService
	calls map[string]int
}

func newFakeConsul(t *testing.T) (*fakeConsul, *api.Client) {
	t.Helper()

	f := &fakeConsul{
		svcs:  map[string][]*api.CatalogService{},
		calls: map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	c, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("api.NewClient: %v", err)
	}
	return f, c
}

func (f *fakeConsul) set(name string, svcs ...*api.CatalogService) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.svcs[name] = svcs
}

func (f *fakeConsul) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	const prefix = "/v1/catalog/service/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	f.calls[name]++

	svcs := f.svcs[name]
	if svcs == nil {
		svcs = []*api.CatalogService{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(svcs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func testConsulCfg() *config.Consul {
	return &config.Consul{
		TagPrefix:       "urlprefix-",
		ServiceStatus:   []string{"passing"},
		ServiceMonitors: 4,
	}
}

func catalogService(node, id, name, addr string, port int, tags ...string) *api.CatalogService {
	return &api.CatalogService{
		Node:           node,
		Address:        addr,
		ServiceID:      id,
		ServiceName:    name,
		ServiceAddress: addr,
		ServicePort:    port,
		ServiceTags:    tags,
	}
}

func passingCheck(node, id, name string, tags ...string) *api.HealthCheck {
	return &api.HealthCheck{
		Node:        node,
		CheckID:     "service:" + id,
		Status:      "passing",
		ServiceID:   id,
		ServiceName: name,
		ServiceTags: tags,
	}
}

// TestMakeConfigDropsBadTagOnlyForItsInstance walks the acceptance sequence:
//
//  1. api on n1 has the bad tags "urlprefix-/api weight=abc" and
//     "urlprefix-/api/v2". Only the bad line is dropped.
//  2. an unrelated web instance goes away: no drop related output.
//  3. a second api instance on n2 passes with the same bad tag: one more WARN.
//  4. the n1 instance fixes its tag: one INFO.
//  5. the n2 instance is deregistered: one INFO.
//
// Web routes must be unaffected through the whole sequence.
func TestMakeConfigDropsBadTagOnlyForItsInstance(t *testing.T) {
	fake, client := newFakeConsul(t)
	sm := NewServiceMonitor(client, testConsulCfg(), "dc1")

	logs := captureLog(t)

	const badTag = "urlprefix-/api weight=abc"
	const fixedTag = "urlprefix-/api weight=1"

	webTags := []string{"urlprefix-/web"}

	// step 1
	fake.set("web",
		catalogService("n1", "web-1", "web", "10.0.0.1", 8080, webTags...),
		catalogService("n2", "web-2", "web", "10.0.0.2", 8080, webTags...),
	)
	fake.set("api",
		catalogService("n1", "api-1", "api", "10.0.1.1", 9090, badTag, "urlprefix-/api/v2"),
	)
	cfg := sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "web-1", "web", webTags...),
		passingCheck("n2", "web-2", "web", webTags...),
		passingCheck("n1", "api-1", "api", badTag, "urlprefix-/api/v2"),
	})
	if got, want := cfg, "route add web /web http://10.0.0.2:8080/\n"+
		"route add web /web http://10.0.0.1:8080/\n"+
		"route add api /api/v2 http://10.0.1.1:9090/"; got != want {
		t.Errorf("step 1 config:\ngot  %q\nwant %q", got, want)
	}
	if got, want := logs.countSinceLastCheck(kWarn), 1; got != want {
		t.Errorf("step 1 produced %d WARN drop lines, want %d", got, want)
	}

	// step 2: web on n2 goes away. api is unchanged, so no drop output.
	cfg = sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "web-1", "web", webTags...),
		passingCheck("n1", "api-1", "api", badTag, "urlprefix-/api/v2"),
	})
	if got, want := cfg, "route add web /web http://10.0.0.1:8080/\n"+
		"route add api /api/v2 http://10.0.1.1:9090/"; got != want {
		t.Errorf("step 2 config:\ngot  %q\nwant %q", got, want)
	}
	if got := logs.countSinceLastCheck(kWarn); got != 0 {
		t.Errorf("step 2 produced %d WARN drop lines, want 0", got)
	}

	// step 3: second api instance on n2 becomes healthy with the same bad tag.
	fake.set("api",
		catalogService("n1", "api-1", "api", "10.0.1.1", 9090, badTag, "urlprefix-/api/v2"),
		catalogService("n2", "api-2", "api", "10.0.1.2", 9090, badTag, "urlprefix-/api/v2"),
	)
	cfg = sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "api-1", "api", badTag, "urlprefix-/api/v2"),
		passingCheck("n2", "api-2", "api", badTag, "urlprefix-/api/v2"),
	})
	if got, want := cfg, "route add api /api/v2 http://10.0.1.2:9090/\n"+
		"route add api /api/v2 http://10.0.1.1:9090/"; got != want {
		t.Errorf("step 3 config:\ngot  %q\nwant %q", got, want)
	}

	// step 4: n1 fixes its tag.
	fake.set("api",
		catalogService("n1", "api-1", "api", "10.0.1.1", 9090, fixedTag, "urlprefix-/api/v2"),
		catalogService("n2", "api-2", "api", "10.0.1.2", 9090, badTag, "urlprefix-/api/v2"),
	)
	cfg = sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "api-1", "api", fixedTag, "urlprefix-/api/v2"),
		passingCheck("n2", "api-2", "api", badTag, "urlprefix-/api/v2"),
	})
	if got, want := cfg, "route add api /api/v2 http://10.0.1.2:9090/\n"+
		"route add api /api/v2 http://10.0.1.1:9090/\n"+
		"route add api /api http://10.0.1.1:9090/ weight 1"; got != want {
		t.Errorf("step 4 config:\ngot  %q\nwant %q", got, want)
	}

	// step 5: n2 is deregistered, so its routes vanish and it stops dropping.
	cfg = sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "api-1", "api", fixedTag, "urlprefix-/api/v2"),
	})
	if got, want := cfg, "route add api /api/v2 http://10.0.1.1:9090/\n"+
		"route add api /api http://10.0.1.1:9090/ weight 1"; got != want {
		t.Errorf("step 5 config:\ngot  %q\nwant %q", got, want)
	}

	// Five steps in total: two WARN (n1 entering, n2 entering) and two
	// INFO (n1 leaving after the fix, n2 leaving after deregistration).
	if got, want := logs.count(kWarn), 2; got != want {
		t.Errorf("got %d WARN drop lines, want %d\n%s", got, want, logs.text())
	}
	if got, want := logs.count(kInfo), 2; got != want {
		t.Errorf("got %d INFO drop lines, want %d\n%s", got, want, logs.text())
	}

	// The drop messages must not leak the merged buffer line number or
	// other services' commands.
	for _, line := range logs.lines() {
		if strings.Contains(line, "line ") {
			t.Errorf("drop log contains a buffer line number: %s", line)
		}
		if strings.Contains(line, "route add web") {
			t.Errorf("drop log contains another service's command: %s", line)
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.dropped) != 0 {
		t.Errorf("drop set not empty at the end: %+v", sm.dropped)
	}
}

// logKind selects drop related log lines for counting.
type logKind int

const (
	kWarn logKind = iota
	kInfo
)

// logCapture records log lines written while a test runs.
type logCapture struct {
	mu      sync.Mutex
	records []string
	seen    int
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	log.SetOutput(&captureWriter{c})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return c
}

type captureWriter struct{ c *logCapture }

func (w *captureWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	w.c.records = append(w.c.records, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// dropLines returns the lines which report a dropped or no longer dropped
// route command.
func (c *logCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.records {
		if strings.Contains(l, "route command from service") {
			out = append(out, l)
		}
	}
	return out
}

func (c *logCapture) count(k logKind) int {
	var n int
	for _, l := range c.lines() {
		switch k {
		case kWarn:
			if strings.Contains(l, "[WARN]") {
				n++
			}
		case kInfo:
			if strings.Contains(l, "[INFO]") {
				n++
			}
		}
	}
	return n
}

// countSinceLastCheck returns the number of matching lines since the
// previous call.
func (c *logCapture) countSinceLastCheck(k logKind) int {
	c.mu.Lock()
	start := c.seen
	c.seen = len(c.records)
	c.mu.Unlock()
	return (&logCapture{records: c.records[start:]}).count(k)
}

func (c *logCapture) text() string {
	return strings.Join(c.lines(), "\n")
}

// TestMakeConfigConcurrencyIndependentDrops runs makeConfig with different
// concurrency settings and verifies the produced config is identical.
func TestMakeConfigConcurrencyIndependentDrops(t *testing.T) {
	fake, client := newFakeConsul(t)

	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("svc-%02d", i)
		tag := "urlprefix-/" + name
		if i%2 == 0 {
			tag = "urlprefix-/" + name + " weight=abc"
		}
		fake.set(name, catalogService("n1", name, name, "10.0.0.1", 8080, tag))
	}

	checks := []*api.HealthCheck{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("svc-%02d", i)
		checks = append(checks, passingCheck("n1", name, name, "urlprefix-/"+name))
	}

	var cfgs []string
	for _, n := range []int{1, 2, 4, 16} {
		c := testConsulCfg()
		c.ServiceMonitors = n
		sm := NewServiceMonitor(client, c, "dc1")
		cfgs = append(cfgs, sm.makeConfig(checks))
	}

	for i := 1; i < len(cfgs); i++ {
		if cfgs[i] != cfgs[0] {
			t.Fatalf("config differs for different ServiceMonitors:\n%s\n---\n%s", cfgs[0], cfgs[i])
		}
	}
	if got, want := strings.Count(cfgs[0], "route add svc-"), 10; got != want {
		t.Errorf("got %d valid route commands, want %d:\n%s", got, want, cfgs[0])
	}
}

// TestServiceConfigDropsEveryBadInputClass covers the three classes of bad
// input which a service tag can produce through routecmd.build:
//
//   - a weight value which the parser rejects,
//   - an ordinary tag with a double quote, which makes the generated
//     "tags" argument unparseable,
//   - a path which parses but is not a valid glob pattern.
//
// Only the bad lines of the instance are dropped; its other tags and other
// instances are unaffected. The reported reason carries the original tag
// text and no location from the merged buffer.
func TestServiceConfigDropsEveryBadInputClass(t *testing.T) {
	fake, client := newFakeConsul(t)
	sm := NewServiceMonitor(client, testConsulCfg(), "dc1")
	logs := captureLog(t)

	fake.set("api",
		// bad weight, good sibling tag, and an invalid glob path
		catalogService("n1", "api-1", "api", "10.0.1.1", 9090,
			"urlprefix-/api weight=abc",
			"urlprefix-/api/v2",
			"urlprefix-/[a",
		),
		// an ordinary tag which breaks the generated command
		catalogService("n1", "api-2", "api", "10.0.1.2", 9090,
			"urlprefix-/api",
			`env="prod"`,
		),
		// unaffected instance of the same service
		catalogService("n1", "api-3", "api", "10.0.1.3", 9090, "urlprefix-/api/v3"),
	)
	fake.set("web", catalogService("n1", "web-1", "web", "10.0.0.1", 8080, "urlprefix-/web"))

	checks := []*api.HealthCheck{
		passingCheck("n1", "api-1", "api"),
		passingCheck("n1", "api-2", "api"),
		passingCheck("n1", "api-3", "api"),
		passingCheck("n1", "web-1", "web"),
	}

	cfg := sm.makeConfig(checks)
	want := "route add web /web http://10.0.0.1:8080/\n" +
		"route add api /api/v3 http://10.0.1.3:9090/\n" +
		"route add api /api/v2 http://10.0.1.1:9090/"
	if cfg != want {
		t.Errorf("config:\ngot  %q\nwant %q", cfg, want)
	}

	// One dropped command per broken tag: the bad weight, the bad glob and
	// the line poisoned by the ordinary tag.
	warns := logs.lines()
	if len(warns) != 3 {
		t.Fatalf("got %d drop lines, want 3:\n%s", len(warns), logs.text())
	}

	joined := strings.Join(warns, "\n")
	for _, wantReason := range []string{
		"weight value invalid",
		"syntax error: 'route add' invalid",
		"invalid path",
	} {
		if !strings.Contains(joined, wantReason) {
			t.Errorf("drop reasons do not contain %q:\n%s", wantReason, joined)
		}
	}

	// The raw tag list is reported, not the generated command.
	if strings.Contains(joined, "route add api /api") {
		t.Errorf("drop log contains the generated command:\n%s", joined)
	}
	if !strings.Contains(joined, `env=\"prod\"`) {
		t.Errorf("drop log does not contain the raw ordinary tag:\n%s", joined)
	}

	// Location must not leak from the merged buffer.
	for _, l := range warns {
		if strings.Contains(l, "line ") {
			t.Errorf("drop log contains a buffer line number: %s", l)
		}
	}
}

// TestServiceConfigAllBadTagsInstanceDisappears documents the behaviour
// change: an instance whose entire set of route tags is invalid is dropped
// from the table instead of staying in it with its previous commands.
func TestServiceConfigAllBadTagsInstanceDisappears(t *testing.T) {
	fake, client := newFakeConsul(t)
	sm := NewServiceMonitor(client, testConsulCfg(), "dc1")
	_ = captureLog(t)

	fake.set("api",
		catalogService("n1", "api-1", "api", "10.0.1.1", 9090, "urlprefix-/api weight=abc"),
		catalogService("n1", "api-2", "api", "10.0.1.2", 9090, "urlprefix-/api"),
	)
	cfg := sm.makeConfig([]*api.HealthCheck{
		passingCheck("n1", "api-1", "api"),
		passingCheck("n1", "api-2", "api"),
	})
	if got, want := cfg, "route add api /api http://10.0.1.2:9090/"; got != want {
		t.Errorf("config:\ngot  %q\nwant %q", got, want)
	}
	if strings.Contains(cfg, "10.0.1.1") {
		t.Errorf("instance with only bad tags is still present:\n%s", cfg)
	}
}

// TestServiceConfigReportsRawTagWithSurroundingSpace verifies that the tag
// text from ServiceTags is reported unmodified, including the whitespace
// which routecmd trims away before building the command.
func TestServiceConfigReportsRawTagWithSurroundingSpace(t *testing.T) {
	fake, client := newFakeConsul(t)
	sm := NewServiceMonitor(client, testConsulCfg(), "dc1")
	logs := captureLog(t)

	fake.set("api", catalogService("n1", "api-1", "api", "10.0.1.1", 9090, " urlprefix-/api weight=abc"))
	sm.makeConfig([]*api.HealthCheck{passingCheck("n1", "api-1", "api")})

	lines := logs.lines()
	if len(lines) != 1 {
		t.Fatalf("got %d drop lines, want 1:\n%s", len(lines), logs.text())
	}
	if !strings.Contains(lines[0], `[" urlprefix-/api weight=abc"]`) {
		t.Errorf("raw tag with surrounding space is not reported verbatim: %s", lines[0])
	}
}

// TestMakeConfigDropLogOrderIndependentOfConcurrency verifies that the drop
// log lines are identical for different ServiceMonitors settings, including
// their order.
func TestMakeConfigDropLogOrderIndependentOfConcurrency(t *testing.T) {
	fake, client := newFakeConsul(t)

	const n = 24
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("svc-%02d", i)
		fake.set(name, catalogService("n1", name, name, fmt.Sprintf("10.0.0.%d", i+1), 8080, "urlprefix-/"+name+" weight=abc"))
	}
	var checks []*api.HealthCheck
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("svc-%02d", i)
		checks = append(checks, passingCheck("n1", name, name))
	}

	var sequences []string
	for _, monitors := range []int{1, 2, 4, 16} {
		c := testConsulCfg()
		c.ServiceMonitors = monitors
		sm := NewServiceMonitor(client, c, "dc1")

		logs := captureLog(t)
		sm.makeConfig(checks)
		sequences = append(sequences, logs.text())
	}

	for i := 1; i < len(sequences); i++ {
		if sequences[i] != sequences[0] {
			t.Fatalf("drop log differs for different ServiceMonitors:\n%s\n---\n%s", sequences[0], sequences[i])
		}
	}
	if got, want := strings.Count(sequences[0], "\n"), n-1; got != want {
		t.Errorf("got %d drop lines, want %d:\n%s", got+1, want+1, sequences[0])
	}
}
