// Package model holds the shape of everything pademelon knows about a VM,
// plus the cache the web layer reads from.
package model

import (
	"sync"
	"time"
)

// AgentState says whether we can talk to the QEMU guest agent inside a VM.
type AgentState string

const (
	// AgentAbsent means the domain XML has no guest agent channel at all.
	// Shouldn't happen on TrueNAS, which adds one to every VM, but handle it.
	AgentAbsent AgentState = "absent"

	// AgentDisconnected means the channel is there but nothing is listening
	// inside the guest — qemu-guest-agent isn't installed or isn't running.
	// We read this straight from the XML and skip the agent calls entirely,
	// so an agentless VM costs us nothing.
	AgentDisconnected AgentState = "disconnected"

	// AgentOK means we asked it something and it answered.
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

// Filesystem is one mounted filesystem inside the guest.
type Filesystem struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	UsedBytes  uint64 `json:"usedBytes"`
	TotalBytes uint64 `json:"totalBytes"`
}

// UsedPercent is how full this filesystem is, 0 if we can't tell.
func (f Filesystem) UsedPercent() float64 {
	if f.TotalBytes == 0 {
		return 0
	}
	return float64(f.UsedBytes) / float64(f.TotalBytes) * 100
}

// VM is everything we know about one virtual machine.
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
	MemKnown    bool    `json:"memKnown"` // false when the balloon told us nothing
	CPUPercent  float64 `json:"cpuPercent"`
	CPUKnown    bool    `json:"cpuKnown"` // false on the first poll, no delta yet

	// Guest-side state, via the QEMU guest agent.
	Agent      AgentState `json:"agent"`
	AgentError string     `json:"agentError,omitempty"`
	Hostname   string     `json:"hostname,omitempty"`
	OS         string     `json:"os,omitempty"`
	Kernel     string     `json:"kernel,omitempty"`

	Interfaces  []Iface      `json:"interfaces"`
	Filesystems []Filesystem `json:"filesystems"`

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

// RealInterfaces drops the noise — loopback, docker bridges, veth pairs.
// A VM running Docker reports a dozen interfaces and you care about one.
func (v VM) RealInterfaces() []Iface {
	out := make([]Iface, 0, len(v.Interfaces))
	for _, i := range v.Interfaces {
		if !i.Virtual {
			out = append(out, i)
		}
	}
	return out
}

// PrimaryIPs is the short answer to "what's this box's address" — every
// non-virtual IPv4 we found, in interface order.
func (v VM) PrimaryIPs() []string {
	var out []string
	for _, i := range v.RealInterfaces() {
		out = append(out, i.IPv4...)
	}
	return out
}

// Snapshot is one complete poll result, and what the JSON API hands out.
type Snapshot struct {
	VMs       []VM      `json:"vms"`
	Polled    time.Time `json:"polled"`
	PollMS    int64     `json:"pollMs"`
	Connected bool      `json:"connected"`
	Error     string    `json:"error,omitempty"`
}

// Cache holds the last good Snapshot. The poller writes it, HTTP handlers
// read it. We never poll on a request — a slow guest agent must not turn
// into a slow page load.
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
