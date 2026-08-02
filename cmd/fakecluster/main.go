// Command fakecluster serves just enough Proxmox and Prometheus API for a local
// energy-watchdog to run against: the real binary, the real code paths, no mocking layer and
// no hardware. Boots and shutdowns take a few seconds so the UI's waiting states are actually
// visible. See the local development section of the README.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bootDelay is how long the fake node and guests take to change state, so "waking the
// server…" and "starting the VM…" are on screen long enough to look at.
const bootDelay = 6 * time.Second

type guest struct {
	VMID    int
	Name    string
	Type    string // qemu | lxc
	Running bool
	GPU     string // hostpci0 passthrough key, empty for guests with no GPU
}

type cluster struct {
	mu        sync.Mutex
	node      string
	up        bool
	bootAt    time.Time
	guests    map[int]*guest
	surplus   float64
	soc       float64
	tasks     map[string]time.Time // upid -> when it completes
	taskSeq   int
	taskDelay time.Duration // how long a bulk task takes to report done
	shutdown  time.Time     // pending power-off, zero if none
	wakeAt    time.Time     // pending wake-on-lan, zero if none
}

func main() {
	addr := flag.String("addr", ":8006", "listen address")
	node := flag.String("node", "pve-1", "managed node name")
	surplus := flag.Float64("surplus", -300, "solar surplus in watts (negative = deficit)")
	up := flag.Bool("up", true, "start with the node powered on")
	taskDelay := flag.Duration("task-delay", 0, "how long bulk migrate/stop tasks take; real ones run for minutes")
	flag.Parse()

	c := &cluster{
		node: *node, up: *up, surplus: *surplus, soc: 80, taskDelay: *taskDelay,
		guests: map[int]*guest{},
		tasks:  map[string]time.Time{},
	}
	c.bootAt = time.Now().Add(-2 * time.Hour) // old enough not to look freshly booted
	// Two guests in each of the migrate and stop ranges, so a shed actually exercises the
	// round-robin across target nodes and a mixed qemu/lxc bulk stop.
	for _, g := range []*guest{
		{VMID: 101, Name: "talos-cp-1", Type: "qemu", Running: true},
		{VMID: 102, Name: "talos-cp-2", Type: "qemu", Running: true},
		{VMID: 301, Name: "media", Type: "qemu", Running: true},
		{VMID: 302, Name: "paperless", Type: "lxc", Running: true},
		// Both desktop VMs share one GPU, as the real ones do: that is the conflict the UI blocks.
		{VMID: 601, Name: "josef-desktop", Type: "qemu", GPU: "gpu0"},
		{VMID: 602, Name: "guest-desktop", Type: "qemu", GPU: "gpu0"},
	} {
		c.guests[g.VMID] = g
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes", c.handleNodes)
	mux.HandleFunc("/api2/json/cluster/replication", c.handleReplication)
	mux.HandleFunc("/api/v1/query", c.handleQuery)
	mux.HandleFunc("/", c.handleNodePaths)
	// Knobs for driving the UI by hand while it's open in a browser.
	mux.HandleFunc("/fake/surplus", c.setSurplus)
	mux.HandleFunc("/fake/state", c.dump)

	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	log.Printf("fakecluster on %s: node=%s up=%v surplus=%.0fW", *addr, *node, *up, *surplus)
	log.Printf("  curl %s/fake/state             # what the fake thinks is true", host)
	log.Printf("  curl '%s/fake/surplus?w=2000'  # push it into surplus", host)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// settle applies any state changes whose delay has elapsed. bootAt records when the node last
// came up, for uptime; wakeAt is a pending wake. Keeping them separate matters - deriving
// "should be up" from bootAt alone made the node boot again the instant it powered off.
func (c *cluster) settle() {
	now := time.Now()
	if !c.shutdown.IsZero() && now.After(c.shutdown) {
		c.up = false
		c.shutdown = time.Time{}
		for _, g := range c.guests {
			g.Running = false
		}
		log.Printf("node is now off")
	}
	if !c.wakeAt.IsZero() && now.After(c.wakeAt) {
		c.up = true
		c.bootAt = now
		c.wakeAt = time.Time{}
		log.Printf("node is now up")
	}
}

func (c *cluster) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (c *cluster) handleNodes(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settle()
	status, uptime := "offline", int64(0)
	if c.up {
		status = "online"
		uptime = int64(time.Since(c.bootAt).Seconds())
	}
	c.ok(w, []map[string]any{{"node": c.node, "status": status, "uptime": uptime}})
}

func (c *cluster) handleReplication(w http.ResponseWriter, _ *http.Request) {
	c.ok(w, []any{})
}

// handleQuery answers the PromQL the watchdog asks, keyed off the metric name in the query.
func (c *cluster) handleQuery(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := r.URL.Query().Get("query")
	// The watchdog asks for surplus as one expression naming both metrics
	// ("sum(production) - sum(consumption)"), so that case has to be matched before either
	// metric alone. The fake serves raw watts, so config.local.yaml sets powerScale: 1.
	prod, cons := strings.Contains(q, "production"), strings.Contains(q, "consumption")
	var v float64
	switch {
	case strings.Contains(q, "charge_level"), strings.Contains(q, "battery"):
		v = c.soc
	case prod && cons:
		v = c.surplus
	case prod:
		v = max(c.surplus, 0) + 500
	case cons:
		v = 500 - min(c.surplus, 0)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"%g"]}]}}`, v)
}

// handleNodePaths covers everything under /api2/json/nodes/<node>/...
func (c *cluster) handleNodePaths(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/")
	if p == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settle()

	switch {
	case parts[1] == "qemu" || parts[1] == "lxc":
		if len(parts) == 2 {
			c.listGuests(w, parts[1])
			return
		}
		c.guestPath(w, parts)
	case parts[1] == "startall" || parts[1] == "stopall" || parts[1] == "migrateall":
		c.bulkAction(w, r, parts[1])
	case parts[1] == "wakeonlan":
		c.wakeAt = time.Now().Add(bootDelay)
		log.Printf("wake-on-lan: node comes up in %s", bootDelay)
		c.ok(w, "cc:28:aa:0e:59:f3")
	case parts[1] == "status":
		c.shutdown = time.Now().Add(bootDelay)
		log.Printf("shutdown: node goes down in %s", bootDelay)
		c.ok(w, nil)
	case parts[1] == "tasks" && len(parts) >= 4:
		c.taskStatus(w, parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (c *cluster) listGuests(w http.ResponseWriter, typ string) {
	var out []map[string]any
	if !c.up {
		c.ok(w, out)
		return
	}
	for _, g := range c.guests {
		if g.Type != typ {
			continue
		}
		status := "stopped"
		if g.Running {
			status = "running"
		}
		out = append(out, map[string]any{"vmid": g.VMID, "name": g.Name, "status": status})
	}
	c.ok(w, out)
}

// guestPath handles /nodes/<node>/<type>/<vmid>/{config,status/<action>}.
func (c *cluster) guestPath(w http.ResponseWriter, parts []string) {
	vmid, err := strconv.Atoi(parts[2])
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	g, ok := c.guests[vmid]
	if !ok {
		http.Error(w, "no such guest", http.StatusNotFound)
		return
	}
	if parts[len(parts)-1] == "config" {
		cfg := map[string]any{"name": g.Name, "cores": 8, "memory": 16384}
		if g.GPU != "" {
			cfg["hostpci0"] = "mapping=" + g.GPU + ",pcie=1,x-vga=1"
		}
		c.ok(w, cfg)
		return
	}
	switch parts[len(parts)-1] {
	case "shutdown", "stop":
		g.Running = false
		log.Printf("guest %d (%s) %s", vmid, g.Name, parts[len(parts)-1])
	case "reboot", "reset":
		g.Running = true
		log.Printf("guest %d (%s) %s", vmid, g.Name, parts[len(parts)-1])
	default:
		http.NotFound(w, nil)
		return
	}
	c.ok(w, c.newTask())
}

// bulkAction handles /nodes/<node>/{startall,stopall,migrateall}. The real thing paces guests
// by startup order and max_workers; here they all take effect at once.
func (c *cluster) bulkAction(w http.ResponseWriter, r *http.Request, action string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// An absent vms filter means "every guest on the node" - the mistake the client guards
	// against, so refuse it here too rather than quietly wiping the fake cluster.
	list := r.Form.Get("vms")
	if list == "" {
		http.Error(w, "refusing a bulk action with no vms filter", http.StatusBadRequest)
		return
	}
	for _, s := range strings.Split(list, ",") {
		vmid, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		g, ok := c.guests[vmid]
		if !ok {
			continue
		}
		switch action {
		case "startall":
			g.Running = true
			log.Printf("guest %d (%s) started", vmid, g.Name)
		case "stopall":
			g.Running = false
			log.Printf("guest %d (%s) stopped", vmid, g.Name)
		case "migrateall":
			delete(c.guests, vmid)
			log.Printf("guest %d (%s) migrated to %s", vmid, g.Name, r.Form.Get("target"))
		}
	}
	c.ok(w, c.newTaskAfter(c.taskDelay))
}

func (c *cluster) newTask() string { return c.newTaskAfter(0) }

// newTaskAfter registers a task that reports done only once d has passed.
func (c *cluster) newTaskAfter(d time.Duration) string {
	c.taskSeq++
	upid := fmt.Sprintf("UPID:%s:fake:%d", c.node, c.taskSeq)
	c.tasks[upid] = time.Now().Add(d)
	return upid
}

func (c *cluster) taskStatus(w http.ResponseWriter, upid string) {
	done, ok := c.tasks[upid]
	if !ok {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	if time.Now().Before(done) {
		c.ok(w, map[string]any{"status": "running"})
		return
	}
	c.ok(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
}

func (c *cluster) setSurplus(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.ParseFloat(r.URL.Query().Get("w"), 64)
	if err != nil {
		http.Error(w, "want ?w=<watts>", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.surplus = v
	c.mu.Unlock()
	log.Printf("surplus set to %.0fW", v)
	_, _ = fmt.Fprintf(w, "surplus=%g\n", v)
}

func (c *cluster) dump(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settle()
	running := map[int]bool{}
	for id, g := range c.guests {
		running[id] = g.Running
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodeUp": c.up, "surplus": c.surplus, "soc": c.soc, "guests": running,
	})
}
