package consul

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/consul/api"
)

// fakeKV serves the KV list endpoint for a fixed set of key/value pairs.
func fakeKVClient(t *testing.T, pairs map[string]string) *api.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/kv/"
		if r.URL.Path != prefix+"fabio/config" {
			http.NotFound(w, r)
			return
		}
		var out api.KVPairs
		for k, v := range pairs {
			out = append(out, &api.KVPair{Key: k, Value: []byte(v), ModifyIndex: 7})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)

	c, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("api.NewClient: %v", err)
	}
	return c
}

// TestListKVKeepsRawContent verifies that the value is not trimmed before
// it is merged, so that a line number reported for invalid content matches
// the raw content shown in the Consul UI.
func TestListKVKeepsRawContent(t *testing.T) {
	c := fakeKVClient(t, map[string]string{
		"fabio/config/maint": "\n  route add svc /maint http://5.6.7.8:5000  \n\n",
	})

	got, _, err := listKV(c, "/fabio/config", 0, true, false, false)
	if err != nil {
		t.Fatalf("listKV: %v", err)
	}

	want := "# --- fabio/config/maint\n\n  route add svc /maint http://5.6.7.8:5000  \n\n"
	if got != want {
		t.Errorf("listKV:\ngot  %q\nwant %q", got, want)
	}
}

// TestListKVWithoutSeparatorTrims verifies the no-route-HTML path is
// unchanged: without the separator the value is still trimmed.
func TestListKVWithoutSeparatorTrims(t *testing.T) {
	c := fakeKVClient(t, map[string]string{"fabio/config/maint": "  <html>  "})

	got, _, err := listKV(c, "/fabio/config", 0, false, false, false)
	if err != nil {
		t.Fatalf("listKV: %v", err)
	}
	if want := "<html>"; got != want {
		t.Errorf("listKV = %q, want %q", got, want)
	}
}
