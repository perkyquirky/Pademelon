// Package libvirtsrc talks to libvirt on the TrueNAS host and turns what it
// finds into model.Snapshot values.
package libvirtsrc

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"pademelon/internal/agent"
	"pademelon/internal/clocks"
	"pademelon/internal/model"
)

// Memory stat tags from libvirt-domain.h. We spell them out rather than
// leaning on the generated constants so the meaning is visible at the point
// of use — these numbers are stable libvirt ABI.
const (
	memStatUnused     int32 = 4 // MemFree in the guest
	memStatAvailable  int32 = 5 // MemTotal in the guest
	memStatUsable     int32 = 8 // MemAvailable — free plus reclaimable cache
	memStatLastUpdate int32 = 9 // host unix time the stats were collected
)

// guestAgentChannel is the virtio-serial channel TrueNAS adds to every VM.
const guestAgentChannel = "org.qemu.guest_agent.0"

// domainNameRE splits TrueNAS's "<vm_id>_<vm_name>" domain naming.
var domainNameRE = regexp.MustCompile(`^(\d+)_(.+)$`)

// Config is everything the Source needs to know.
type Config struct {
	// Socket is the libvirt unix socket. On TrueNAS this is the non-standard
	// /run/truenas_libvirt/libvirt-sock, not /var/run/libvirt/libvirt-sock.
	Socket string

	// AgentTimeout is how long to give a guest agent command. libvirt's API
	// takes whole seconds; main rejects sub-second values at startup, so
	// one wedged guest can't stall a poll.
	AgentTimeout time.Duration

	// StatsPeriod is how often QEMU re-collects balloon stats from the
	// guest. QEMU only collects while its poll timer runs, and TrueNAS's
	// domain XML never sets one, so without it every reading is a
	// single snapshot taken at guest boot. 0 disables and keeps that
	// boot-snapshot behaviour (with stale readings rejected).
	StatsPeriod time.Duration

	// Concurrency caps how many VMs we interrogate at once.
	Concurrency int

	Log *slog.Logger
}

// Source is a live connection to libvirt, plus the small amount of state we
// need to work out rates of change between polls.
type Source struct {
	cfg Config
	log *slog.Logger

	mu   sync.Mutex
	conn *libvirt.Libvirt

	// prevCPU holds the last CPU time sample per domain, so we can turn
	// libvirt's cumulative nanosecond counter into a percentage.
	prevCPU map[string]cpuSample

	// prevBlock and prevNet hold the previous poll's cumulative byte
	// counters per disk and NIC, keyed by domain and device — the same
	// delta-between-polls trick the CPU column uses, applied to throughput.
	prevBlock map[string]blockSample
	prevNet   map[string]netSample

	// lastAgent remembers each VM's agent state so we can log the transition
	// once instead of moaning about the same missing agent every 30 seconds.
	lastAgent map[string]model.AgentState

	// lastMemStale remembers which VMs we have already warned about stale
	// balloon stats, so a wedged guest costs one log line, not one per poll.
	lastMemStale map[string]bool

	// statsPeriodSet remembers for which VMs we have switched on QEMU's
	// balloon stats poll timer during this boot of each VM.
	statsPeriodSet map[string]bool
}

type cpuSample struct {
	cpuTime uint64
	at      time.Time
}

type blockSample struct {
	rdBytes, wrBytes uint64
	at               time.Time
}

type netSample struct {
	rxBytes, txBytes uint64
	at               time.Time
}

// New builds a Source. It does not connect — Poll does that on demand and
// reconnects if the socket goes away.
func New(cfg Config) *Source {
	if cfg.AgentTimeout <= 0 {
		cfg.AgentTimeout = clocks.DefaultAgentTimeout
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	// A balloon reading is roughly one to two collection periods old by the
	// time we look at it — QEMU collects on its own timer, we read whenever
	// the poll fires. Once 2× the period reaches the staleness threshold,
	// most readings get rejected as fossils and the memory column shows
	// "allocated". That's a config mistake worth complaining about once.
	if cfg.StatsPeriod > 0 && 2*cfg.StatsPeriod >= clocks.BalloonStaleAfter {
		cfg.Log.Warn("stats period close to staleness threshold, most memory readings will be rejected",
			"stats_period", cfg.StatsPeriod,
			"stale_after", clocks.BalloonStaleAfter,
		)
	}
	return &Source{
		cfg:            cfg,
		log:            cfg.Log,
		prevCPU:        map[string]cpuSample{},
		prevBlock:      map[string]blockSample{},
		prevNet:        map[string]netSample{},
		lastAgent:      map[string]model.AgentState{},
		lastMemStale:   map[string]bool{},
		statsPeriodSet: map[string]bool{},
	}
}

// Close drops the libvirt connection.
func (s *Source) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Disconnect()
		s.conn = nil
	}
}

// connect returns a live libvirt handle, dialling if needed.
//
// /run is tmpfs, so the socket directory is rebuilt at boot and libvirtd
// recreates the socket when it restarts. Reconnecting rather than assuming
// the handle stays good is what lets the container survive a NAS reboot
// without needing a restart itself.
func (s *Source) connect() (*libvirt.Libvirt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil && s.conn.IsConnected() {
		return s.conn, nil
	}
	if s.conn != nil {
		_ = s.conn.Disconnect()
		s.conn = nil
	}

	l := libvirt.NewWithDialer(dialers.NewLocal(
		dialers.WithSocket(s.cfg.Socket),
		dialers.WithLocalTimeout(10*time.Second),
	))
	if err := l.Connect(); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", s.cfg.Socket, err)
	}
	s.log.Info("connected to libvirt", "socket", s.cfg.Socket)
	s.conn = l
	return l, nil
}

// Poll gathers the current state of every VM.
func (s *Source) Poll() (model.Snapshot, error) {
	start := time.Now()

	conn, err := s.connect()
	if err != nil {
		return model.Snapshot{}, err
	}

	domains, _, err := conn.ConnectListAllDomains(1, 0)
	if err != nil {
		// Almost always means the connection died under us. Drop it so the
		// next poll redials rather than retrying on a corpse.
		s.Close()
		return model.Snapshot{}, fmt.Errorf("list domains: %w", err)
	}

	vms := make([]model.VM, len(domains))
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup

	for i, d := range domains {
		wg.Add(1)
		go func(i int, d libvirt.Domain) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			vms[i] = s.inspect(conn, d)
		}(i, d)
	}
	wg.Wait()

	withAgent := 0
	for _, v := range vms {
		if v.Agent == model.AgentOK {
			withAgent++
		}
	}

	took := time.Since(start)
	s.log.Info("poll ok",
		"vms", len(vms),
		"with_agent", withAgent,
		"took", took.Round(time.Millisecond),
	)

	return model.Snapshot{
		VMs:       vms,
		Polled:    time.Now(),
		PollMS:    took.Milliseconds(),
		Connected: true,
	}, nil
}

// inspect gathers everything about one domain. It never returns an error —
// a VM we can't fully read still gets a row with whatever we did manage,
// because "this VM exists and is running but the agent is quiet" is useful
// information, not a failure.
func (s *Source) inspect(conn *libvirt.Libvirt, d libvirt.Domain) model.VM {
	vm := model.VM{
		Domain:      d.Name,
		Name:        d.Name,
		Interfaces:  []model.Iface{},
		Filesystems: []model.Filesystem{},
		Updated:     time.Now(),
	}

	// TrueNAS names domains "<vm_id>_<vm_name>". Split it so the UI can show
	// "test" rather than "12_test". Note this id is TrueNAS's, and is not the
	// same number as libvirt's runtime d.ID.
	if m := domainNameRE.FindStringSubmatch(d.Name); m != nil {
		if id, err := strconv.Atoi(m[1]); err == nil {
			vm.ID = id
			vm.Name = m[2]
		}
	}

	state, maxMem, _, vcpus, cpuTime, err := conn.DomainGetInfo(d)
	if err != nil {
		s.log.Warn("domain info failed", "domain", d.Name, "err", err)
		vm.State = "unknown"
		return vm
	}

	vm.State = stateName(state)
	vm.Running = libvirt.DomainState(state) == libvirt.DomainRunning
	vm.VCPUs = int(vcpus)
	vm.MemTotalKiB = maxMem

	// The XML is worth fetching for every VM, running or not: disks, NICs
	// and the agent channel state all live in it. A stopped VM reports the
	// shapes (which disk, which bus) but libvirt can't answer capacity or
	// rate questions about a machine that isn't running.
	xmlDesc, err := conn.DomainGetXMLDesc(d, 0)
	if err != nil {
		s.log.Warn("domain xml failed", "domain", d.Name, "err", err)
		if !vm.Running {
			vm.Agent = model.AgentAbsent
			return vm
		}
		vm.Agent = model.AgentError
		vm.AgentError = err.Error()
		return vm
	}
	vm.XML = xmlDesc

	dx := parseDomainXML(xmlDesc)
	vm.UUID = dx.UUID
	vm.Disks = diskShapes(&dx)
	vm.Nics = nicShapes(&dx)

	if !vm.Running {
		// A stopped VM has nothing else to tell us, but it still belongs in
		// the list — vanishing when you shut one down is exactly the thing
		// that makes an in-guest monitoring tool useless for this job.
		s.forgetCPU(d.Name)
		s.forgetDomainSamples(d.Name)
		s.forgetStatsPeriod(d.Name)
		vm.Agent = model.AgentAbsent
		return vm
	}

	vm.CPUPercent, vm.CPUKnown = s.cpuPercent(d.Name, cpuTime, int(vcpus))

	s.enableStatsPeriod(conn, d)

	if used, total, ok := s.memory(conn, d); ok {
		vm.MemUsedKiB = used
		vm.MemKnown = true
		if total > 0 {
			vm.MemTotalKiB = total
		}
	}

	// Rates and capacity are host-side numbers: they work even when the
	// guest has no agent installed at all.
	now := time.Now()
	for i := range vm.Disks {
		s.fillDiskLive(conn, d, &vm.Disks[i], now)
	}
	for i := range vm.Nics {
		s.fillNicLive(conn, d, &vm.Nics[i], now)
	}

	// Read the channel state out of the XML before calling the agent. A VM
	// without qemu-guest-agent installed shows state='disconnected', and
	// skipping it here is the difference between a poll that costs nothing
	// and one that burns the full agent timeout on every agentless VM.
	switch agentChannelState(&dx) {
	case "":
		vm.Agent = model.AgentAbsent
	case "connected":
		s.fillFromAgent(conn, d, &vm)
		joinNicGuestNames(&vm)
	default:
		vm.Agent = model.AgentDisconnected
	}

	s.logAgentTransition(d.Name, vm.Agent)
	return vm
}

// fillDiskLive asks libvirt for everything a running disk can tell us: its
// capacity, and read/write throughput across the last two polls. It mutes
// itself on any failure — a disk we can't read rates for simply keeps its
// shape and no numbers.
func (s *Source) fillDiskLive(conn *libvirt.Libvirt, d libvirt.Domain, disk *model.Disk, now time.Time) {
	_, capacity, _, err := conn.DomainGetBlockInfo(d, disk.Dev, 0)
	if err != nil {
		s.log.Debug("block info failed", "domain", d.Name, "dev", disk.Dev, "err", err)
	} else {
		disk.CapacityBytes = capacity
	}

	key := d.Name + "\x00" + disk.Dev
	_, rd, _, wr, _, err := conn.DomainBlockStats(d, disk.Dev)
	if err != nil {
		s.log.Debug("block stats failed", "domain", d.Name, "dev", disk.Dev, "err", err)
		return
	}
	s.mu.Lock()
	prev, had := s.prevBlock[key]
	s.prevBlock[key] = blockSample{rdBytes: uint64(rd), wrBytes: uint64(wr), at: now}
	s.mu.Unlock()

	if rd < 0 || wr < 0 {
		// libvirt hands back -1 for counters it can't fill; forget any
		// previous sample so the next good poll starts fresh rather than
		// dividing against nonsense.
		s.mu.Lock()
		delete(s.prevBlock, key)
		s.mu.Unlock()
		return
	}
	if !had {
		return // first poll for this disk: nothing to difference against
	}
	elapsed := now.Sub(prev.at)
	var okRd, okWr bool
	disk.RdBytesPS, okRd = bytesPerSecond(prev.rdBytes, uint64(rd), elapsed)
	disk.WrBytesPS, okWr = bytesPerSecond(prev.wrBytes, uint64(wr), elapsed)
	disk.RatesKnown = okRd && okWr
}

// fillNicLive is fillDiskLive's network twin: receive/send throughput for
// one tap device across the last two polls.
func (s *Source) fillNicLive(conn *libvirt.Libvirt, d libvirt.Domain, nic *model.Nic, now time.Time) {
	if nic.Device == "" {
		// No tap device in the XML — libvirt has no counter for it.
		return
	}
	key := d.Name + "\x00" + nic.Device
	rx, _, _, _, tx, _, _, _, err := conn.DomainInterfaceStats(d, nic.Device)
	if err != nil {
		s.log.Debug("interface stats failed", "domain", d.Name, "dev", nic.Device, "err", err)
		return
	}
	s.mu.Lock()
	prev, had := s.prevNet[key]
	s.prevNet[key] = netSample{rxBytes: uint64(rx), txBytes: uint64(tx), at: now}
	s.mu.Unlock()

	if rx < 0 || tx < 0 {
		s.mu.Lock()
		delete(s.prevNet, key)
		s.mu.Unlock()
		return
	}
	if !had {
		return
	}
	elapsed := now.Sub(prev.at)
	var okRx, okTx bool
	nic.RxBytesPS, okRx = bytesPerSecond(prev.rxBytes, uint64(rx), elapsed)
	nic.TxBytesPS, okTx = bytesPerSecond(prev.txBytes, uint64(tx), elapsed)
	nic.RatesKnown = okRx && okTx
}

// bytesPerSecond differences two samples of a cumulative byte counter.
// ok=false when the counter went backwards — the VM restarted and the old
// sample belongs to a previous lifetime (the same rule cpuPercent uses).
func bytesPerSecond(prev, nowv uint64, elapsed time.Duration) (uint64, bool) {
	if elapsed <= 0 || nowv < prev {
		return 0, false
	}
	return uint64(float64(nowv-prev) / elapsed.Seconds()), true
}

// forgetDomainSamples drops one domain's rate history. Counters are
// cumulative per QEMU instance, so a stopped or restarted VM's old samples
// are from a different lifetime.
func (s *Source) forgetDomainSamples(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.prevBlock {
		if strings.HasPrefix(k, domain+"\x00") {
			delete(s.prevBlock, k)
		}
	}
	for k := range s.prevNet {
		if strings.HasPrefix(k, domain+"\x00") {
			delete(s.prevNet, k)
		}
	}
}

// fillFromAgent runs the guest agent queries. Each one is best-effort: a
// guest that answers guest-get-osinfo but not guest-get-fsinfo should still
// show its OS.
func (s *Source) fillFromAgent(conn *libvirt.Libvirt, d libvirt.Domain, vm *model.VM) {
	call := s.agentCaller(conn, d)

	if err := agent.Ping(call); err != nil {
		vm.Agent = model.AgentError
		vm.AgentError = err.Error()
		return
	}
	vm.Agent = model.AgentOK

	if host, err := agent.Hostname(call); err == nil {
		vm.Hostname = host
	} else {
		s.log.Debug("hostname failed", "domain", d.Name, "err", err)
	}

	if osName, kernel, err := agent.OSInfo(call); err == nil {
		vm.OS, vm.Kernel = osName, kernel
	} else {
		s.log.Debug("osinfo failed", "domain", d.Name, "err", err)
	}

	if ifaces, err := agent.Interfaces(call); err == nil {
		vm.Interfaces = ifaces
	} else {
		s.log.Debug("interfaces failed", "domain", d.Name, "err", err)
	}

	if fs, err := agent.Filesystems(call); err == nil {
		vm.Filesystems = fs
	} else {
		s.log.Debug("fsinfo failed", "domain", d.Name, "err", err)
	}

	// The version line turns "why doesn't this VM show X?" into a one-
	// glance answer; the clock drift flags paused or restored VMs whose
	// guest clock NTP hasn't caught up with yet. Both are best-effort.
	if version, _, err := agent.Info(call); err == nil {
		vm.AgentVersion = version
	} else {
		s.log.Debug("agent info failed", "domain", d.Name, "err", err)
	}

	if nsec, err := agent.Time(call); err == nil {
		vm.ClockDriftSeconds = time.Duration(nsec - time.Now().UnixNano()).Seconds()
		vm.ClockDriftKnown = true
	} else {
		s.log.Debug("guest time failed", "domain", d.Name, "err", err)
	}
}

// secondsInt32 converts a duration to the whole seconds libvirt's agent and
// stats APIs take. main rejects sub-second values at startup, so this only
// ever sees whole seconds.
func secondsInt32(d time.Duration) int32 {
	return int32(d / time.Second)
}

// agentCaller adapts libvirt's agent RPC to the agent package's Caller.
func (s *Source) agentCaller(conn *libvirt.Libvirt, d libvirt.Domain) agent.Caller {
	return func(cmd string) (string, error) {
		res, err := conn.QEMUDomainAgentCommand(d, cmd, secondsInt32(s.cfg.AgentTimeout), 0)
		if err != nil {
			return "", err
		}
		if len(res) == 0 {
			return "", fmt.Errorf("empty agent reply")
		}
		return res[0], nil
	}
}

// memory reads guest memory from the virtio balloon.
//
// used is worked out as total minus MemAvailable where the guest reports it,
// which is what `free` calls used. Falling back to total minus MemFree counts
// the page cache as used and makes every healthy Linux box look full.
func (s *Source) memory(conn *libvirt.Libvirt, d libvirt.Domain) (used, total uint64, ok bool) {
	stats, err := conn.DomainMemoryStats(d, 16, 0)
	if err != nil {
		s.log.Debug("memory stats failed", "domain", d.Name, "err", err)
		return 0, 0, false
	}

	used, total, ok, stale := balloonMemory(stats, time.Now())
	s.logMemoryStale(d.Name, stale)
	return used, total, ok
}

// balloonMemory turns raw balloon stats into used/total KiB. ok=false means
// the guest told us nothing usable — the caller then shows the VM as
// "allocated" rather than inventing a used value. stale is a special case of
// !ok: the stats are a dated snapshot from a guest whose balloon driver has
// stopped answering, not a live reading.
//
// QEMU stamps the stats with the host clock when the guest driver answers,
// so there is no guest clock skew to worry about.
func balloonMemory(stats []libvirt.DomainMemoryStat, now time.Time) (used, total uint64, ok, stale bool) {
	var available, unused, usable uint64
	var haveAvailable, haveUnused, haveUsable bool
	var lastUpdate int64
	var haveLastUpdate bool
	for _, st := range stats {
		switch st.Tag {
		case memStatAvailable:
			available, haveAvailable = st.Val, true
		case memStatUnused:
			unused, haveUnused = st.Val, true
		case memStatUsable:
			usable, haveUsable = st.Val, true
		case memStatLastUpdate:
			lastUpdate, haveLastUpdate = int64(st.Val), true
		}
	}
	if !haveAvailable {
		return 0, 0, false, false
	}

	if haveLastUpdate && lastUpdate > 0 && now.Sub(time.Unix(lastUpdate, 0)) > clocks.BalloonStaleAfter {
		return 0, 0, false, true
	}

	switch {
	case haveUsable && usable <= available:
		return available - usable, available, true, false
	case haveUnused && unused <= available:
		return available - unused, available, true, false
	default:
		return 0, 0, false, false
	}
}

// cpuPercent turns libvirt's cumulative CPU nanoseconds into a percentage of
// the VM's allocated cores. Returns ok=false on the first poll for a domain,
// when there's nothing to compare against yet.
func (s *Source) cpuPercent(domain string, cpuTime uint64, vcpus int) (float64, bool) {
	now := time.Now()

	s.mu.Lock()
	prev, had := s.prevCPU[domain]
	s.prevCPU[domain] = cpuSample{cpuTime: cpuTime, at: now}
	s.mu.Unlock()

	if !had || vcpus <= 0 {
		return 0, false
	}
	elapsed := now.Sub(prev.at).Nanoseconds()
	if elapsed <= 0 || cpuTime < prev.cpuTime {
		// Counter went backwards, so the VM restarted. Skip this sample.
		return 0, false
	}

	pct := float64(cpuTime-prev.cpuTime) / float64(elapsed) * 100 / float64(vcpus)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

func (s *Source) forgetCPU(domain string) {
	s.mu.Lock()
	delete(s.prevCPU, domain)
	s.mu.Unlock()
}

// enableStatsPeriod starts QEMU's internal balloon stats polling for a
// running VM, once per boot of that VM.
//
// QEMU only re-collects balloon stats while its poll timer runs, and that
// timer exists only when the domain XML sets <memballoon><stats period> or
// someone makes this call. TrueNAS never sets one, so without it every
// query — ours, virsh's, anything's — returns the single snapshot the guest
// pushed at boot, and the memory column shows numbers from boot time
// forever.
//
// This is the one deliberate exception to "only read APIs". It sets no
// memory, adds no devices and enters no guest: it flips a polling knob
// inside QEMU so the guest's balloon driver starts answering stats requests.
// It is the same class of privileged-but-harmless as the guest agent queries
// this tool already makes, the CI grep whitelists this call name and nothing
// else, and README2.md "About the permissions" documents it.
func (s *Source) enableStatsPeriod(conn *libvirt.Libvirt, d libvirt.Domain) {
	if s.cfg.StatsPeriod <= 0 {
		return
	}

	s.mu.Lock()
	done := s.statsPeriodSet[d.Name]
	s.statsPeriodSet[d.Name] = true
	s.mu.Unlock()
	if done {
		return
	}

	if err := conn.DomainSetMemoryStatsPeriod(d, secondsInt32(s.cfg.StatsPeriod), 0); err != nil {
		// One transient failure shouldn't disable the feature for the
		// process's lifetime; try again on the next poll.
		s.mu.Lock()
		delete(s.statsPeriodSet, d.Name)
		s.mu.Unlock()
		s.log.Debug("stats period failed", "domain", d.Name, "err", err)
		return
	}
	s.log.Debug("balloon stats polling enabled", "domain", d.Name, "stats_period", s.cfg.StatsPeriod)
}

// forgetStatsPeriod drops the per-boot marker. A VM restart makes a fresh
// QEMU with the poll timer off again, so the next boot must re-apply it.
func (s *Source) forgetStatsPeriod(domain string) {
	s.mu.Lock()
	delete(s.statsPeriodSet, domain)
	s.mu.Unlock()
}

// logAgentTransition logs only when a VM's agent state changes, so a box
// without the agent installed doesn't write a line every single poll.
func (s *Source) logAgentTransition(domain string, now model.AgentState) {
	s.mu.Lock()
	prev, had := s.lastAgent[domain]
	s.lastAgent[domain] = now
	s.mu.Unlock()

	if had && prev == now {
		return
	}
	switch {
	case now == model.AgentOK && had:
		s.log.Info("guest agent back", "domain", domain, "was", prev)
	case now == model.AgentOK:
		s.log.Info("guest agent responding", "domain", domain)
	case now == model.AgentDisconnected:
		s.log.Warn("guest agent not running in guest", "domain", domain,
			"hint", "install the QEMU guest agent (qemu-guest-agent on Linux, virtio-win guest tools on Windows)")
	case now == model.AgentError:
		s.log.Warn("guest agent channel open but not answering", "domain", domain)
	}
}

// logMemoryStale warns once per transition when a VM's balloon stats have
// gone stale. A guest whose virtio_balloon driver stopped answering would
// otherwise either spam this every poll or stay completely silent while the
// dashboard shows days-old numbers.
func (s *Source) logMemoryStale(domain string, stale bool) {
	s.mu.Lock()
	prev := s.lastMemStale[domain]
	s.lastMemStale[domain] = stale
	s.mu.Unlock()

	if prev == stale {
		return
	}
	if stale {
		s.log.Warn("guest memory stats stale, hiding RAM usage", "domain", domain,
			"hint", "the balloon driver in the guest stopped reporting; restarting the VM usually unsticks it")
		return
	}
	if prev {
		s.log.Info("guest memory stats fresh again", "domain", domain)
	}
}

// domainXML is the slice of a domain's XML we actually care about.
type domainXML struct {
	XMLName xml.Name `xml:"domain"`
	UUID    string   `xml:"uuid"`
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"` // disk, cdrom, floppy, lun
			Driver struct {
				Type string `xml:"type,attr"` // raw, qcow2, ...
			} `xml:"driver"`
			Source struct {
				Dev  string `xml:"dev,attr"`  // block source (zvol)
				File string `xml:"file,attr"` // file source (qcow2, ISO)
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"` // vda
				Bus string `xml:"bus,attr"` // virtio
			} `xml:"target"`
		} `xml:"disk"`
		Interfaces []struct {
			Type string `xml:"type,attr"` // bridge, network, direct, ...
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Bridge string `xml:"bridge,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"` // the host-side tap device
			} `xml:"target"`
		} `xml:"interface"`
		Channels []struct {
			Target struct {
				Type  string `xml:"type,attr"`
				Name  string `xml:"name,attr"`
				State string `xml:"state,attr"`
			} `xml:"target"`
		} `xml:"channel"`
	} `xml:"devices"`
}

// parseDomainXML pulls everything we care about out of a domain's XML.
// A parse failure returns a zero value, which the caller treats as "this VM
// exists but tells us nothing" rather than dropping the row.
func parseDomainXML(raw string) (dx domainXML) {
	_ = xml.Unmarshal([]byte(raw), &dx)
	return dx
}

// agentChannelState reads the guest agent channel's state out of the parsed
// XML. Empty means the VM has no guest agent channel at all.
func agentChannelState(dx *domainXML) string {
	for _, ch := range dx.Devices.Channels {
		if ch.Target.Name == guestAgentChannel {
			return ch.Target.State
		}
	}
	return ""
}

// diskShapes turns the XML disk list into model.Disks. Everything that
// isn't real storage gets dropped — cdroms and floppies report through the
// same list and would drown the actual disks, the same way squashfs mounts
// drown real filesystems in the storage column.
func diskShapes(dx *domainXML) []model.Disk {
	out := make([]model.Disk, 0, len(dx.Devices.Disks))
	for _, xd := range dx.Devices.Disks {
		if xd.Device != "disk" || xd.Target.Dev == "" {
			continue // installation media, or a disk too shapeless to show
		}
		source := xd.Source.Dev
		if source == "" {
			source = xd.Source.File
		}
		out = append(out, model.Disk{
			Dev:    xd.Target.Dev,
			Source: source,
			Format: xd.Driver.Type,
			Bus:    xd.Target.Bus,
		})
	}
	return out
}

// nicShapes turns the XML interface list into model.Nics. Only the
// interface types that back a real guest NIC make the cut.
func nicShapes(dx *domainXML) []model.Nic {
	out := make([]model.Nic, 0, len(dx.Devices.Interfaces))
	for _, xi := range dx.Devices.Interfaces {
		switch xi.Type {
		case "bridge", "network", "direct", "ethernet":
		default:
			continue // user-mode slirp and friends aren't the guest's NIC
		}
		out = append(out, model.Nic{
			Device: xi.Target.Dev,
			MAC:    strings.ToLower(xi.MAC.Address),
			Bridge: xi.Source.Bridge,
		})
	}
	return out
}

// joinNicGuestNames labels each NIC with the guest's own interface name by
// matching MACs against the agent's interface list. The tap device is what
// the host calls it; "eth0" is what the guest calls it, and the guest's
// name is the one humans recognise.
func joinNicGuestNames(vm *model.VM) {
	for i := range vm.Nics {
		for _, iface := range vm.Interfaces {
			if iface.MAC != "" && strings.EqualFold(iface.MAC, vm.Nics[i].MAC) {
				vm.Nics[i].GuestName = iface.Name
				break
			}
		}
	}
}

func stateName(state uint8) string {
	switch libvirt.DomainState(state) {
	case libvirt.DomainRunning:
		return "running"
	case libvirt.DomainBlocked:
		return "blocked"
	case libvirt.DomainPaused:
		return "paused"
	case libvirt.DomainShutdown:
		return "shutting down"
	case libvirt.DomainShutoff:
		return "stopped"
	case libvirt.DomainCrashed:
		return "crashed"
	case libvirt.DomainPmsuspended:
		return "suspended"
	default:
		return "unknown"
	}
}
