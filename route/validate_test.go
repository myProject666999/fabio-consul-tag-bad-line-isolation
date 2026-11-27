package route

import (
	"bytes"
	"reflect"
	"testing"

	gkm "github.com/go-kit/kit/metrics"
	"github.com/go-kit/kit/metrics/discard"

	"github.com/fabiolb/fabio/metrics"
)

// validCommands are commands which Parse and NewTable both accept. The
// validator must accept them as well.
var validRouteCommands = []string{
	`route add svc /prefix http://1.2.3.4/`,
	`route add svc /prefix http://1.2.3.4/ weight 1.2`,
	`route add svc /prefix http://1.2.3.4/ weight 1.2 tags "a,b"`,
	`route add svc /prefix http://1.2.3.4/ opts "strip=/path prepend=/new register=alpha"`,
	`route add svc :1234 tcp://1.2.3.4:5678`,
	`route add svc /*/prefix http://1.2.3.4/`,
	`route del svc`,
	`route del svc /prefix`,
	`route del svc /prefix http://1.2.3.4/`,
	`route del tags "a,b"`,
	`route weight svc /prefix weight 1.2`,
	``,
	`# comment`,
	`// comment`,
}

// invalidRouteCommands are commands which Parse or Table.addRoute reject.
var invalidRouteCommands = []string{
	`bang`,
	`route x`,
	`route add`,
	`route add svc`,
	// parseWeight rejects a non numeric weight. The line still matches
	// reAdd, so this only fails deep inside the parser.
	`route add svc /api http://1.2.3.4/ weight abc`,
	// strconv.Quote of a tag containing a double quote produces an
	// escaped tags string, which the tags capture group of reAdd
	// ([^"]) cannot match.
	`route add svc /api http://1.2.3.4/ tags "env=\"prod\""`,
	// The path is a valid command token but not a valid glob pattern.
	// Parse accepts it and addRoute fails, so a Parse-only validator
	// would miss it.
	`route add svc /[a http://1.2.3.4/`,
	// Embedded line breaks would split a single generated command into
	// several commands in the merged buffer.
	"route add svc /api http://1.2.3.4/\nroute add svc /evil http://6.6.6.6/",
}

func TestValidateRouteCommand(t *testing.T) {
	for _, cmd := range validRouteCommands {
		if err := ValidateRouteCommand(cmd); err != nil {
			t.Errorf("ValidateRouteCommand(%q) = %v, want nil", cmd, err)
		}
	}
	for _, cmd := range invalidRouteCommands {
		if err := ValidateRouteCommand(cmd); err == nil {
			t.Errorf("ValidateRouteCommand(%q) = nil, want error", cmd)
		}
	}
}

// TestValidateRouteCommandMatchesParseAndTable checks that the validator
// agrees with the real pipeline for every command: a command is accepted
// by the validator exactly when the command is accepted by Parse and by
// building a one line table from it.
func TestValidateRouteCommandMatchesParseAndTable(t *testing.T) {
	for _, cmd := range append(validRouteCommands, invalidRouteCommands...) {
		if hasLineBreak(cmd) {
			// Parse splits the input into several commands by design, so
			// there is no single command to compare with.
			continue
		}

		_, parseErr := Parse(bytes.NewBufferString(cmd))
		_, tableErr := NewTable(bytes.NewBufferString(cmd))
		valid := ValidateRouteCommand(cmd) == nil

		if want := parseErr == nil && tableErr == nil; valid != want {
			t.Errorf("ValidateRouteCommand(%q) valid=%v, parse=%v table=%v", cmd, valid, parseErr, tableErr)
		}
	}
}

func hasLineBreak(s string) bool {
	return bytes.ContainsAny([]byte(s), "\r\n")
}

// TestValidateRouteCommandErrorsHaveNoLineNumber makes sure the validator
// does not report a location: the caller knows the line and must be able
// to add its own, e.g. the KV key and line, without a second line number.
func TestValidateRouteCommandErrorsHaveNoLineNumber(t *testing.T) {
	for _, cmd := range invalidRouteCommands {
		err := ValidateRouteCommand(cmd)
		if err == nil {
			continue
		}
		if got := err.Error(); bytes.ContainsAny([]byte(got), "\r\n") || containsLinePrefix(got) {
			t.Errorf("error for %q leaks location: %q", cmd, got)
		}
	}
}

func containsLinePrefix(s string) bool {
	p := "line "
	return len(s) >= len(p) && s[:len(p)] == p
}

// countingProvider wraps metrics.Provider and counts With calls made by
// targets which were created.
type countingProvider struct {
	mu *int
}

func (p countingProvider) NewCounter(name string, labels ...string) gkm.Counter {
	return countingCounter{mu: p.mu}
}
func (p countingProvider) NewGauge(name string, labels ...string) gkm.Gauge {
	return discard.NewGauge()
}
func (p countingProvider) NewHistogram(name string, labels ...string) gkm.Histogram {
	return countingHistogram{mu: p.mu}
}

type countingCounter struct{ mu *int }

func (c countingCounter) With(labelValues ...string) gkm.Counter {
	*c.mu++
	return c
}
func (c countingCounter) Add(float64) {}

type countingHistogram struct{ mu *int }

func (h countingHistogram) With(labelValues ...string) gkm.Histogram {
	*h.mu++
	return h
}
func (h countingHistogram) Observe(float64) {}

// TestValidateRouteCommandNoSideEffects checks that validation creates no
// Target: the abandoned approach (parse one line, then run addRoute on a
// temporary table) calls counters.With for every validated line and creates
// a transport for a command which combines host and https, leaving series in
// the metrics backend of a route which was never added.
func TestValidateRouteCommandNoSideEffects(t *testing.T) {
	var withCalls int
	SetMetricsProvider(countingProvider{mu: &withCalls})
	t.Cleanup(func() { SetMetricsProvider(metrics.DiscardProvider{}) })

	tableBefore := GetTable()

	cmds := []string{
		`route add svc /api https://1.2.3.4/ opts "host=example.com proto=https register=alias"`,
		`route add svc /api http://1.2.3.4/ weight abc`,
		`route add svc /[a http://1.2.3.4/`,
	}
	for _, cmd := range cmds {
		_ = ValidateRouteCommand(cmd)
	}

	if withCalls != 0 {
		t.Fatalf("validator created %d metric series", withCalls)
	}
	if got, want := GetTable(), tableBefore; !reflect.DeepEqual(got, want) {
		t.Fatalf("validator modified the routing table")
	}
}
