// Package snapshots fetches zvol snapshot lists from the TrueNAS
// middleware on demand. Nothing here runs on a timer: a fetch starts when
// a panel opens, when the refresh button asks, or when the action layer
// needs to verify a snapshot id. The middleware answers one VM at a time
// — its per-dataset queries are the expensive part (a dataset with years
// of auto-* snapshots), so concurrent fetches would stack that cost.
package snapshots

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"pademelon/internal/clocks"
	"pademelon/internal/model"
	"pademelon/internal/truenas"
)

// Lister is the slice of the middleware client the service needs: its
// connection status and the per-dataset snapshot list.
type Lister interface {
	Status() truenas.Status
	Snapshots(ctx context.Context, dataset string) ([]truenas.Snapshot, error)
}

// Waker pokes the middleware client into dialling now instead of waiting
// out its reconnect backoff. Optional: nil just means a fetch after a
// dropout fails and the next one tries again.
type Waker interface {
	Wake()
}

// Config is what New needs. The durations fall back to the clocks
// constants when zero; injectable so tests do not wait on real seconds.
type Config struct {
	Lister Lister            // required: the middleware reads
	Waker  Waker             // optional: reconnect-now hint
	VMs    func() []model.VM // required: what the poller last saw

	Budget time.Duration // one VM's whole fetch; clocks.SnapshotFetchBudget
	Floor  time.Duration // minimum gap between fetches for one VM; clocks.SnapshotRefetchFloor
	Grace  time.Duration // wait for a reconnect after Wake; clocks.SnapshotConnectGrace

	Log *slog.Logger
}

// State is the read-side view of one VM's snapshot list. The web layer
// turns it into the JSON the panel renders.
type State struct {
	Domain    string
	Snapshots []model.ZfsSnapshot
	Status    truenas.Status // middleware status at the last fetch attempt
	FetchedAt time.Time      // last fetch attempt, success or failure
	Fetching  bool           // a fetch is queued or running
	Error     string         // why the last attempt failed; empty when clean
	Stale     bool           // the list is carried over from an earlier fetch
}

// state is the service's own record for one VM.
type state struct {
	snapshots []model.ZfsSnapshot
	status    truenas.Status
	fetchedAt time.Time
	err       string
	stale     bool
	fetching  bool
}

// snapshot returns a copy safe to hand past the lock.
func (st *state) snapshot(domain string) State {
	out := State{
		Domain:    domain,
		Status:    st.status,
		FetchedAt: st.fetchedAt,
		Fetching:  st.fetching,
		Error:     st.err,
		Stale:     st.stale,
	}
	if st.snapshots != nil {
		out.Snapshots = make([]model.ZfsSnapshot, len(st.snapshots))
		copy(out.Snapshots, st.snapshots)
	}
	return out
}

// Service owns the per-VM snapshot lists and the one-at-a-time fetch
// slot. Handlers call Request; a refresh sweep calls RefreshAll; the
// action layer calls ValidateKnown. None of them block on the middleware
// except ValidateKnown, which has a deadline.
type Service struct {
	cfg    Config
	log    *slog.Logger
	budget time.Duration
	floor  time.Duration
	grace  time.Duration

	mu       sync.Mutex
	states   map[string]*state
	sweeping bool

	// fetchMu is the one-at-a-time rule: every middleware fetch for
	// every VM crosses this mutex. Held for one VM's whole fetch, so a
	// sweep and a panel-open fetch interleave VM by VM, never overlap.
	fetchMu sync.Mutex
}

// New returns a Service. It dials nothing and fetches nothing until the
// first Request or RefreshAll.
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Budget <= 0 {
		cfg.Budget = clocks.SnapshotFetchBudget
	}
	if cfg.Floor <= 0 {
		cfg.Floor = clocks.SnapshotRefetchFloor
	}
	if cfg.Grace <= 0 {
		cfg.Grace = clocks.SnapshotConnectGrace
	}
	return &Service{
		cfg:    cfg,
		log:    cfg.Log,
		budget: cfg.Budget,
		floor:  cfg.Floor,
		grace:  cfg.Grace,
		states: map[string]*state{},
	}
}

// Request returns one VM's snapshot list. A force request starts a fetch
// unless one is already coming or the last one finished within the
// refetch floor; a non-force request never starts anything and answers
// from what is already held. The middleware is never contacted on this
// goroutine — the fetch runs in its own goroutine, and the caller polls
// (or repaints on the next tick) until Fetching turns false.
func (s *Service) Request(domain string, force bool) State {
	s.mu.Lock()
	st := s.states[domain]
	if st == nil {
		st = &state{}
		s.states[domain] = st
	}
	if force && !st.fetching {
		tooSoon := !st.fetchedAt.IsZero() && st.err == "" && time.Since(st.fetchedAt) < s.floor
		if !tooSoon {
			st.fetching = true
			go s.fetch(domain)
		}
	}
	out := st.snapshot(domain)
	s.mu.Unlock()
	return out
}

// RefreshAll queues one fetch for every VM the poller knows and returns
// how many it queued. The fetches run one VM at a time, in order; VMs
// already mid-fetch are skipped, and a sweep that is still running makes
// a second call a no-op. State for VMs the poller no longer reports is
// dropped.
func (s *Service) RefreshAll() int {
	vms := s.cfg.VMs()
	known := make(map[string]bool, len(vms))
	for _, vm := range vms {
		known[vm.Domain] = true
	}

	s.mu.Lock()
	for d := range s.states {
		if !known[d] {
			delete(s.states, d)
		}
	}
	if s.sweeping {
		s.mu.Unlock()
		return 0
	}
	var queued []string
	for _, vm := range vms {
		st := s.states[vm.Domain]
		if st != nil {
			if st.fetching {
				continue // already coming
			}
			// A clean fetch inside the refetch floor is fresh enough;
			// re-sweeping it would only hammer the middleware.
			if st.err == "" && !st.fetchedAt.IsZero() && time.Since(st.fetchedAt) < s.floor {
				continue
			}
		}
		if st == nil {
			st = &state{}
			s.states[vm.Domain] = st
		}
		st.fetching = true
		queued = append(queued, vm.Domain)
	}
	if len(queued) == 0 {
		s.mu.Unlock()
		return 0
	}
	s.sweeping = true
	s.mu.Unlock()

	go func() {
		for _, d := range queued {
			s.fetch(d)
		}
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
	}()
	return len(queued)
}

// ValidateKnown answers whether the middleware knows this snapshot id for
// this VM right now. A clean held list that contains the id answers
// immediately; otherwise it starts a fetch and waits (bounded by ctx) for
// the answer, so restore and delete submissions are checked against live
// middleware state, not a stale cache. A failed fetch is an error, never
// a silent "unknown" — the caller refuses the submission either way.
func (s *Service) ValidateKnown(ctx context.Context, domain, id string) (bool, error) {
	s.mu.Lock()
	st := s.states[domain]
	if st != nil && !st.fetching && st.err == "" && containsID(st.snapshots, id) {
		s.mu.Unlock()
		return true, nil
	}
	if st == nil {
		st = &state{}
		s.states[domain] = st
	}
	if !st.fetching {
		st.fetching = true
		go s.fetch(domain)
	}
	s.mu.Unlock()

	for {
		s.mu.Lock()
		st = s.states[domain]
		if st == nil {
			s.mu.Unlock()
			return false, fmt.Errorf("VM %s is no longer known to the poller", domain)
		}
		fetching, ferr, snaps := st.fetching, st.err, st.snapshots
		s.mu.Unlock()

		if !fetching {
			if ferr != "" {
				return false, fmt.Errorf("cannot verify the snapshot list: %s", ferr)
			}
			return containsID(snaps, id), nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("the snapshot check did not finish in time")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func containsID(list []model.ZfsSnapshot, id string) bool {
	for _, sn := range list {
		if sn.ID == id {
			return true
		}
	}
	return false
}

// fetch runs one VM's fetch. The caller has set fetching under the lock;
// fetch always clears it. It serializes on fetchMu, re-reads the poller's
// VM list (it may have refreshed while this fetch waited for its turn),
// queries each zvol dataset in sequence, and pours the rows into the
// state. The fetch context comes from Background, not from any request:
// the result belongs to the service, and a browser closing its connection
// must not kill a fetch other readers are waiting on.
func (s *Service) fetch(domain string) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	var datasets []string
	found := false
	for _, vm := range s.cfg.VMs() {
		if vm.Domain == domain {
			found = true
			for _, disk := range vm.Disks {
				if ds, ok := truenas.DatasetFromDiskSource(disk.Source); ok {
					datasets = append(datasets, ds)
				}
			}
			break
		}
	}

	s.mu.Lock()
	st := s.states[domain]
	if st == nil || !found {
		// The VM vanished, or the state was dropped while this fetch
		// waited. Nothing to fill.
		delete(s.states, domain)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	status := s.cfg.Lister.Status()
	if !status.Connected && len(datasets) > 0 {
		// Pull the reconnect forward, then give it a short grace so a
		// fetch opened right after a dropout still succeeds.
		if s.cfg.Waker != nil {
			s.cfg.Waker.Wake()
		}
		deadline := time.Now().Add(s.grace)
		for !status.Connected && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			status = s.cfg.Lister.Status()
		}
	}

	var (
		list     []model.ZfsSnapshot
		firstErr string
	)
	if len(datasets) > 0 && status.Connected {
		gctx, cancel := context.WithTimeout(context.Background(), s.budget)
		defer cancel()
		for _, ds := range datasets {
			if gctx.Err() != nil {
				if firstErr == "" {
					firstErr = "snapshot fetch ran out of time"
				}
				break
			}
			rows, err := s.cfg.Lister.Snapshots(gctx, ds)
			if err != nil {
				if firstErr == "" {
					firstErr = err.Error()
				}
				continue
			}
			for _, r := range rows {
				list = append(list, model.ZfsSnapshot{
					ID: r.ID, Dataset: r.Dataset, Name: r.Name,
					Created: r.Created, Used: r.Used, Referenced: r.Referenced,
				})
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Created > list[b].Created })
	}

	// The error the panel shows. When the connection is down, its own
	// reason (a refused dial, a failed login) beats the generic
	// "middleware not connected" the queries would report.
	msg := firstErr
	if !status.Connected && len(datasets) > 0 {
		switch {
		case status.Error != "":
			msg = status.Error
		case msg == "":
			msg = truenas.ErrNotConnected.Error()
		}
	}

	s.mu.Lock()
	st = s.states[domain]
	if st == nil {
		s.mu.Unlock()
		return
	}
	st.status = status
	st.fetchedAt = time.Now()
	st.fetching = false
	switch {
	case len(datasets) == 0:
		// No zvol disks: no snapshots by definition, and nothing failed.
		st.snapshots = []model.ZfsSnapshot{}
		st.err = ""
		st.stale = false
	case msg == "":
		st.snapshots = list
		st.err = ""
		st.stale = false
	case len(list) > 0:
		// Partial success: show what arrived, say what failed.
		st.snapshots = list
		st.err = msg
		st.stale = false
	case len(st.snapshots) > 0:
		// Total failure with a previous list: keep it, mark it stale.
		// A middleware hiccup reads as "stale", never as "your
		// snapshots vanished".
		st.err = msg
		st.stale = true
	default:
		st.snapshots = nil
		st.err = msg
		st.stale = false
	}
	s.mu.Unlock()

	if msg != "" {
		s.log.Warn("snapshot fetch for VM failed", "domain", domain, "err", msg)
	} else {
		s.log.Debug("snapshot fetch for VM done", "domain", domain, "datasets", len(datasets), "snapshots", len(list))
	}
}
