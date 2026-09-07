// Package model holds the shape of everything pademelon knows about a VM,
// plus the cache the web layer reads from.
package model

import (
	"sync"
	"time"
)

// AgentState says whether Pademelon can talk to the QEMU guest agent inside a VM.
type AgentState string

const (
	// AgentAbsent means the domain XML has no guest agent channel at all.
	// This should not happen on TrueNAS, which adds one to every VM, but
	// handle it.
	AgentAbsent AgentState = "absent"

	// AgentDisconnected means the channel is there but nothing listens
	// inside the guest: qemu-guest-agent is not installed or is not
	// running. The poller reads this state straight from the XML and skips
	// the agent calls entirely, so an agentless VM costs nothing.
	AgentDisconnected AgentState = "disconnected"

	// AgentOK means the poller asked the agent something and it answered.
	AgentOK AgentState = "ok"

	// AgentError means the channel claimed to be connected but the call
	// failed or timed out anyway.
	AgentError AgentState = "error"
)

// Iface is one network interface as the guest sees it.
type Iface struct {
	Name    string   `json:"name"`
	MAC     string   `json:"mac"`
	IPv4    []string `json:"ipv4"`
	IPv6    []string `json:"ipv6"`
	Virtual bool     `json:"virtual"` // loopback, docker bridge, veth, etc
}

// Disk is one virtual disk, as the host side sees it. The shape comes from
// the domain XML. The rates come from libvirt's cumulative block counters,
// converted to bytes-per-second the same way as the CPU column.
type Disk struct {
	Dev           string `json:"dev"`              // vda, sdb, ...
	Source        string `json:"source,omitempty"` // zvol path or file path
	Format        string `json:"format,omitempty"` // raw, qcow2, ...
	Bus           string `json:"bus,omitempty"`    // virtio, sata, ...
	CapacityBytes uint64 `json:"capacityBytes"`    // 0 when unknown
	RdBytesPS     uint64 `json:"rdBytesPs"`
	WrBytesPS     uint64 `json:"wrBytesPs"`
	RatesKnown    bool   `json:"ratesKnown"` // false on the first poll, no delta yet
}

// Nic is one virtual network interface, host side. Device is the tap
// device libvirt made for it; GuestName is filled in when the agent's
// interface list contains a matching MAC, so the row can say "eth0"
// instead of "tap-something".
type Nic struct {
	Device     string `json:"device"`
	MAC        string `json:"mac,omitempty"`
	Bridge     string `json:"bridge,omitempty"`
	GuestName  string `json:"guestName,omitempty"`
	RxBytesPS  uint64 `json:"rxBytesPs"`
	TxBytesPS  uint64 `json:"txBytesPs"`
	RatesKnown bool   `json:"ratesKnown"`
}

// Filesystem is one mounted filesystem inside the guest.
type Filesystem struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	UsedBytes  uint64 `json:"usedBytes"`
	TotalBytes uint64 `json:"totalBytes"`
}

// UsedPercent is how full this filesystem is, 0 when unknown.
func (f Filesystem) UsedPercent() float64 {
	if f.TotalBytes == 0 {
		return 0
	}
	return float64(f.UsedBytes) / float64(f.TotalBytes) * 100
}

// ZfsSnapshot is one zvol snapshot as TrueNAS sees it. Pademelon-made
// snapshots and periodic-task ones are the same ZFS objects — the name is
// the only difference (pademelon-<vm>-<ts> versus auto-*), which is what
// makes them visible and manageable in the TrueNAS UI too.
type ZfsSnapshot struct {
	ID         string `json:"id"`      // dataset@name
	Dataset    string `json:"dataset"` // nvme/vms/alpine_test-bxuwle
	Name       string `json:"name"`    // snapshot name alone
	Created    int64  `json:"created"` // epoch seconds
	Used       uint64 `json:"used"`    // copy-on-write divergence, bytes
	Referenced uint64 `json:"referenced"`
}

// TruenasGather rides each Snapshot (the poll result) and says how the
// middleware snapshot gathering went this round. Nil means the
// integration is off.
type TruenasGather struct {
	Connected bool      `json:"connected"`
	Version   string    `json:"version,omitempty"`
	Error     string    `json:"error,omitempty"` // this round's gather failure, if any
	At        time.Time `json:"at"`
}

// VM is everything Pademelon knows about one virtual machine.
type VM struct {
	// Identity. Domain is what libvirt calls it ("12_test"); ID and Name are
	// that split apart, because TrueNAS names domains "<vm_id>_<vm_name>".
	Domain string `json:"domain"`
	ID     int    `json:"id"`
	Name   string `json:"name"`
	UUID   string `json:"uuid"`

	// Host-side state. Always available, even for a VM with no agent.
	State   string `json:"state"`
	Running bool   `json:"running"`

	VCPUs       int     `json:"vcpus"`
	MemTotalKiB uint64  `json:"memTotalKiB"`
	MemUsedKiB  uint64  `json:"memUsedKiB"`
	MemKnown    bool    `json:"memKnown"` // false when the balloon gave no reading
	CPUPercent  float64 `json:"cpuPercent"`
	CPUKnown    bool    `json:"cpuKnown"` // false on the first poll, no delta yet

	// Guest-side state, via the QEMU guest agent.
	Agent      AgentState `json:"agent"`
	AgentError string     `json:"agentError,omitempty"`
	Hostname   string     `json:"hostname,omitempty"`
	OS         string     `json:"os,omitempty"`
	Kernel     string     `json:"kernel,omitempty"`

	// AgentVersion is what guest-info calls itself, for example "8.2" — it
	// answers "why doesn't this VM show X?" in one glance.
	// ClockDriftSeconds is the guest clock minus the host clock at poll
	// time; positive means the guest is ahead. A paused or recently
	// restored VM drifts; a healthy one stays within a second or two of
	// noise.
	AgentVersion      string  `json:"agentVersion,omitempty"`
	ClockDriftSeconds float64 `json:"clockDriftSeconds"`
	ClockDriftKnown   bool    `json:"clockDriftKnown"`

	Interfaces  []Iface      `json:"interfaces"`
	Filesystems []Filesystem `json:"filesystems"`

	// Snapshots holds the zvol snapshots of this VM's disk datasets,
	// gathered by the poll loop from the TrueNAS middleware when the
	// integration is on. Deliberately not in the JSON — a dataset with
	// years of periodic snapshots would bloat /api/vms, which is fetched
	// every 1.5s; only /api/vm/{name}/snapshots serves them.
	Snapshots []ZfsSnapshot `json:"-"`

	// Host-side shapes from the domain XML, with rates filled in while the
	// VM runs. These work whether or not the guest has an agent.
	Disks []Disk `json:"disks"`
	Nics  []Nic  `json:"nics"`

	// XML is the raw domain definition from the last poll, served by
	// /api/vm/{name}/xml. Storing it keeps the rule true: the poller talks
	// to libvirt, and handlers read the cache. It is deliberately not in
	// the JSON — only that route serves it, and shipping every VM's full
	// XML on every /api/vms fetch would waste bytes.
	XML string `json:"-"`

	Updated time.Time `json:"updated"`
	Stale   bool      `json:"stale"` // last poll failed, showing older data
}

// MemUsedPercent is how much of its allocated RAM the guest is using.
func (v VM) MemUsedPercent() float64 {
	if !v.MemKnown || v.MemTotalKiB == 0 {
		return 0
	}
	return float64(v.MemUsedKiB) / float64(v.MemTotalKiB) * 100
}

// RealInterfaces drops the noise: loopback, docker bridges, veth pairs.
// A VM that runs Docker reports a dozen interfaces, and only one matters
// to the user.
func (v VM) RealInterfaces() []Iface {
	out := make([]Iface, 0, len(v.Interfaces))
	for _, i := range v.Interfaces {
		if !i.Virtual {
			out = append(out, i)
		}
	}
	return out
}

// PrimaryIPs answers "what is this box's address" in short form: every
// non-virtual IPv4 address, in interface order.
func (v VM) PrimaryIPs() []string {
	var out []string
	for _, i := range v.RealInterfaces() {
		out = append(out, i.IPv4...)
	}
	return out
}

// Snapshot is one complete poll result, and what the JSON API hands out.
type Snapshot struct {
	VMs       []VM           `json:"vms"`
	Polled    time.Time      `json:"polled"`
	PollMS    int64          `json:"pollMs"`
	Connected bool           `json:"connected"`
	Error     string         `json:"error,omitempty"`
	Truenas   *TruenasGather `json:"truenas,omitempty"` // nil when the middleware integration is off
}

// Cache holds the last good Snapshot. The poller writes it, HTTP handlers
// read it. The server never polls on a request — a slow guest agent must
// not turn into a slow page load.
type Cache struct {
	mu   sync.RWMutex
	snap Snapshot
}

// NewCache returns an empty cache that reports itself as disconnected until
// the first poll lands.
func NewCache() *Cache {
	return &Cache{snap: Snapshot{VMs: []VM{}}}
}

// Set replaces the cached snapshot.
func (c *Cache) Set(s Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = s
}

// Get returns the cached snapshot.
func (c *Cache) Get() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

// SetError marks the cache as disconnected but keeps the VM list, so the page
// shows stale data with a warning rather than going blank when libvirt drops.
func (c *Cache) SetError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap.Connected = false
	c.snap.Error = err.Error()
	for i := range c.snap.VMs {
		c.snap.VMs[i].Stale = true
	}
}
