package main

import (
	"strings"
	"testing"
)

func TestValidateManualConfig(t *testing.T) {
	const kvPath = "/fabio/config"

	tests := []struct {
		desc string
		in   string
		err  string // "" means valid
	}{
		{
			desc: "empty",
			in:   "",
		},
		{
			desc: "blank lines and comments",
			in:   "# --- fabio/config\n\n# a comment\nroute add svc /a http://1.2.3.4/\n",
		},
		{
			desc: "comment which looks like a separator stays content",
			in:   "# --- fabio/config\n# --- disabled\nroute add svc /a http://1.2.3.4/\n",
		},
		{
			desc: "two keys, valid",
			in:   "# --- fabio/config\nroute add svc /a http://1.2.3.4/\n\n# --- fabio/config/maint\nroute del svc /a\n",
		},
		{
			desc: "invalid first line of first key",
			in:   "# --- fabio/config\nbang\n",
			err:  `key "fabio/config" line 1`,
		},
		{
			desc: "invalid second line names the key and the line",
			in:   "# --- fabio/config\nroute add svc /a http://1.2.3.4/\nroute add svc /[a http://1.2.3.4/\n",
			err:  `key "fabio/config" line 2`,
		},
		{
			desc: "invalid line in the second key restarts numbering",
			in: "# --- fabio/config\nroute add svc /a http://1.2.3.4/\n\n" +
				"# --- fabio/config/maint\n\nroute add svc /maint http://5.6.7.8/ weight abc\n",
			err: `key "fabio/config/maint" line 2`,
		},
		{
			desc: "leading blank line is counted",
			in:   "# --- fabio/config\n\nbang\n",
			err:  `key "fabio/config" line 2`,
		},
		{
			desc: "line without a key is reported by line only",
			in:   "bang\n",
			err:  "line 1",
		},
		{
			desc: "double quote in value breaks the line",
			in:   "# --- fabio/config\nroute add svc /a http://1.2.3.4/ tags \"env=\\\"prod\\\"\"\n",
			err:  "line 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := validateManualConfig(kvPath, tt.in)
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("got error %v, want nil", err)
			case tt.err != "" && err == nil:
				t.Errorf("got nil, want error containing %q", tt.err)
			case tt.err != "" && !strings.Contains(err.Error(), tt.err):
				t.Errorf("got error %q, want it to contain %q", err, tt.err)
			}
		})
	}
}

// TestValidateManualConfigIgnoresLookalikeSeparator verifies that a content
// line starting with "# --- " is treated as a content line and not as the
// separator of a new key, so the reported key and line stay correct.
func TestValidateManualConfigIgnoresLookalikeSeparator(t *testing.T) {
	in := "# --- fabio/config\n" +
		"# --- disabled\n" +
		"route add svc /[a http://1.2.3.4/\n"

	err := validateManualConfig("/fabio/config", in)
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if want := `key "fabio/config" line 2`; !strings.Contains(err.Error(), want) {
		t.Errorf("got error %q, want it to contain %q", err, want)
	}
}
