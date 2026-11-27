package consul

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fabiolb/fabio/config"
	"github.com/fabiolb/fabio/route"
	"github.com/hashicorp/consul/api"
)

// ServiceMonitor generates fabio configurations from consul state.
type ServiceMonitor struct {
	client *api.Client
	config *config.Consul
	dc     string
	strict bool

	// mu guards dropped. dropped holds the route commands which were
	// rejected during the last makeConfig call, keyed by a stable
	// identity, so that repeated health changes do not repeat log output.
	mu      sync.Mutex
	dropped map[string]droppedCommand
}

func NewServiceMonitor(client *api.Client, config *config.Consul, dc string) *ServiceMonitor {
	return &ServiceMonitor{
		client: client,
		config: config,
		dc:     dc,
		strict: config.ChecksRequired == "all",
	}
}

// Watch monitors the consul health checks and sends a new
// configuration to the updates channel on every change.
func (w *ServiceMonitor) Watch(updates chan string) {
	var lastIndex uint64
	var q *api.QueryOptions
	for {
		if w.config.PollInterval != 0 {
			q = &api.QueryOptions{RequireConsistent: w.config.RequireConsistent, AllowStale: w.config.AllowStale}
			time.Sleep(w.config.PollInterval)
		} else {
			q = &api.QueryOptions{RequireConsistent: w.config.RequireConsistent, AllowStale: w.config.AllowStale, WaitIndex: lastIndex}
		}
		checks, meta, err := w.client.Health().State("any", q)
		if err != nil {
			log.Printf("[WARN] consul: Error fetching health state. %v", err)
			time.Sleep(time.Second)
			continue
		}
		log.Printf("[DEBUG] consul: Health changed to #%d", meta.LastIndex)

		prefixedChecks := checksWithTagPrefix(w.config.TagPrefix, checks)
		log.Printf("[DEBUG] consul: only %d of %d checks have the configured tag prefix", len(prefixedChecks), len(checks))

		// determine which services have passing health checks
		passing := passingServices(prefixedChecks, w.config.ServiceStatus, w.strict)

		// build the config for the passing services
		updates <- w.makeConfig(passing)

		// remember the last state and wait for the next change
		lastIndex = meta.LastIndex
	}
}

// makeConfig determines which service instances have passing health checks
// and then finds the ones which have tags with the right prefix to build the config from.
func (w *ServiceMonitor) makeConfig(checks []*api.HealthCheck) string {
	// map service name to list of service passing for which the health check is ok
	m := map[string]map[string]bool{}
	for _, check := range checks {
		// Make the node part of the id, because according to the Consul docs
		// the ServiceID is unique per agent but not cluster wide
		// https://www.consul.io/api/agent/service.html#id
		name, id := check.ServiceName, fmt.Sprintf("%s.%s", check.Node, check.ServiceID)

		if _, ok := m[name]; !ok {
			m[name] = map[string]bool{}
		}
		m[name][id] = true
	}

	n := w.config.ServiceMonitors
	if n <= 0 {
		n = 1
	}

	type result struct {
		config  []string
		dropped []droppedCommand
	}
	sem := make(chan int, n)
	cfgs := make(chan result, len(m))
	for name, passing := range m {
		go func() {
			sem <- 1
			cfg, drops := w.serviceConfig(name, passing)
			cfgs <- result{cfg, drops}
			<-sem
		}()
	}

	var config []string
	var dropped []droppedCommand
	for range m {
		res := <-cfgs
		config = append(config, res.config...)
		dropped = append(dropped, res.dropped...)
	}

	// sort config in reverse order to sort most specific config to the top
	sort.Sort(sort.Reverse(sort.StringSlice(config)))

	// logDropped is called after all service goroutines have finished and
	// sorts the set itself, so its output does not depend on the
	// concurrency configured via ServiceMonitors.
	w.logDropped(dropped)

	return strings.Join(config, "\n")
}

// serviceConfig constructs the config for all good instances of a single service.
//
// Every route command generated from a service tag is validated individually.
// Commands which the route package would reject are dropped and reported
// instead of being passed to the shared configuration buffer, where a single
// bad line would invalidate the whole table.
func (w *ServiceMonitor) serviceConfig(name string, passing map[string]bool) (config []string, dropped []droppedCommand) {
	if name == "" || len(passing) == 0 {
		return nil, nil
	}

	q := &api.QueryOptions{RequireConsistent: w.config.RequireConsistent, AllowStale: w.config.AllowStale}
	svcs, _, err := w.client.Catalog().Service(name, "", q)
	if err != nil {
		log.Printf("[WARN] consul: Error getting catalog service %s. %v", name, err)
		return nil, nil
	}

	env := map[string]string{
		"DC": w.dc,
	}

	for _, svc := range svcs {
		// check if this instance passed the health check
		if _, ok := passing[svc.Node+"."+svc.ServiceID]; !ok {
			continue
		}

		r := routecmd{
			svc:    svc,
			env:    env,
			prefix: w.config.TagPrefix,
		}

		cmds := r.build()
		for _, cmd := range cmds {
			if err := route.ValidateRouteCommand(cmd); err != nil {
				dropped = append(dropped, newDroppedCommand(svc, cmd, err))
				continue
			}
			config = append(config, cmd)
		}
	}
	return config, dropped
}

// droppedCommand describes a route command generated from a service tag
// which the route package rejected.
type droppedCommand struct {
	// key identifies the dropped command for change detection. It contains
	// no buffer line number because the command is validated before it
	// reaches the merged configuration buffer, and it differs per instance
	// so that a second instance with the same bad tag is a new warning.
	key string

	service   string
	node      string
	serviceID string
	// tags are the raw ServiceTags of the instance, not the generated
	// command. An ordinary tag can make the generated command invalid
	// (e.g. a double quote in "env=\"prod\""), so the whole raw tag list
	// is reported and not just the urlprefix tag.
	tags   []string
	reason string
}

// newDroppedCommand records the raw ServiceTags of the instance and a
// reason which carries no location information from the merged
// configuration buffer.
func newDroppedCommand(svc *api.CatalogService, cmd string, err error) droppedCommand {
	tags := append([]string(nil), svc.ServiceTags...)
	return droppedCommand{
		key:       svc.Node + "\x00" + svc.ServiceID + "\x00" + cmd + "\x00" + err.Error(),
		service:   svc.ServiceName,
		node:      svc.Node,
		serviceID: svc.ServiceID,
		tags:      tags,
		reason:    err.Error(),
	}
}

// logDropped remembers the dropped commands of the current evaluation and
// logs them only when the set of dropped commands changed. Commands which
// entered the set are logged as WARN, commands which left it as INFO, no
// matter whether they were fixed or the instance went away.
//
// The output is independent of the concurrency used to build the config
// because the set is ordered deterministically before it is logged.
func (w *ServiceMonitor) logDropped(dropped []droppedCommand) {
	next := make(map[string]droppedCommand, len(dropped))
	for _, d := range dropped {
		next[d.key] = d
	}

	w.mu.Lock()
	current := w.dropped
	w.dropped = next
	w.mu.Unlock()

	sortedByKey := func(m map[string]droppedCommand) []droppedCommand {
		list := make([]droppedCommand, 0, len(m))
		for _, d := range m {
			list = append(list, d)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].key < list[j].key })
		return list
	}

	for _, d := range sortedByKey(next) {
		if _, ok := current[d.key]; !ok {
			log.Printf("[WARN] consul: Dropped invalid route command from service %s on node %s (ServiceID %q) with tags %q: %s",
				d.service, d.node, d.serviceID, d.tags, d.reason)
		}
	}
	for _, d := range sortedByKey(current) {
		if _, ok := next[d.key]; !ok {
			log.Printf("[INFO] consul: No longer dropping invalid route command from service %s on node %s (ServiceID %q) with tags %q",
				d.service, d.node, d.serviceID, d.tags)
		}
	}
}

// checksWithTagPrefix filters a list of Consul Health Checks to only the Checks with a Tag that begins with the prefix
func checksWithTagPrefix(prefix string, checks api.HealthChecks) api.HealthChecks {
	checksWithPrefix := make(api.HealthChecks, 0, len(checks))
	for _, c := range checks {
		if c.CheckID == "serfHealth" || c.CheckID == "_node_maintenance" || strings.HasPrefix(c.CheckID, "_service_maintenance") {
			checksWithPrefix = append(checksWithPrefix, c)
			continue
		}
		for _, t := range c.ServiceTags {
			if strings.HasPrefix(t, prefix) {
				checksWithPrefix = append(checksWithPrefix, c)
				break
			}
		}
	}

	return checksWithPrefix
}
