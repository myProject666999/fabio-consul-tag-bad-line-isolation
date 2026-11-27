package route

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gobwas/glob"
)

// ValidateRouteCommand reports whether a single route command would be
// accepted by Parse and applied by NewTable without error.
//
// It performs the same checks as Parse and Table.addRoute/Table.delRoute but
// without looking at the rest of the configuration, without building a
// routing table and without touching any metrics. It is used to isolate bad
// input so that a single malformed line cannot reject an entire
// configuration.
//
// The returned error carries no location information: the caller knows which
// line it is validating and reports the location itself.
func ValidateRouteCommand(cmd string) error {
	// A route command is always a single line. Reject embedded line breaks
	// so that a generated command cannot turn into several commands when it
	// is merged into the shared configuration buffer.
	if strings.ContainsAny(cmd, "\r\n") {
		return errors.New("syntax error: route command must be a single line")
	}

	def, err := parseLine(cmd)
	if err != nil {
		return err
	}
	if def == nil {
		// blank line or comment
		return nil
	}
	return validateRouteDef(def)
}

// validateRouteDef mirrors the checks which Table applies when a parsed
// RouteDef is added to a table. Keep this in sync with Table.addRoute,
// Table.delRoute and Table.weighRoute.
func validateRouteDef(d *RouteDef) error {
	switch d.Cmd {
	case RouteAddCmd:
		if d.Src == "" {
			return errInvalidPrefix
		}
		if d.Dst == "" {
			return errInvalidTarget
		}
		if _, err := url.Parse(d.Dst); err != nil {
			return fmt.Errorf("route: invalid target. %s", err)
		}
		// addRoute compiles the path as a glob whenever the route is new.
		// A pattern which does not compile can never be added to a table,
		// so compiling it here has no false positives: an identical path
		// can only already exist in a table if it compiled successfully.
		_, path := hostpath(d.Src)
		if _, err := glob.Compile(path); err != nil {
			return fmt.Errorf("route: invalid path %q. %s", path, err)
		}

	case RouteDelCmd:
		// delRoute parses the target only when a service and target are
		// given and no tags filter is used.
		if d.Dst != "" && len(d.Tags) == 0 {
			if _, err := url.Parse(d.Dst); err != nil {
				return fmt.Errorf("route: invalid target. %s", err)
			}
		}

	case RouteWeightCmd:
		// An invalid weight is reported by NewTable but does not abort it.
		// A weight command which matches nothing is only logged there.

	default:
		return fmt.Errorf("route: invalid command: %s", d.Cmd)
	}

	return nil
}
