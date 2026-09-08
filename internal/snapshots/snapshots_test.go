package snapshots

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"pademelon/internal/model"
	"pademelon/internal/truenas"
)

// fakeLister is the Lister surface, canned: a status, per-dataset lists,
// an optional universal error, and an optional per-call delay for the
// budget and serialization tests. It also counts concurrent Snapshots
// calls, which is how the one-VM-at-a-time rule is proven.
type fakeLister struct {
	st    truenas.Status
	lists map[string][]truenas.Snapshot
	err   error
	delay time.Duration

	mu       sync.Mutex
	calls    []string
	inFlight int
	maxSeen  int
}

func (f *fakeLister) Status() truenas.Status { return f.st }

func (f *fakeLister) Snapshots(ctx context.Context, dataset string) ([]truenas.Snapshot, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxSeen {
		f.maxSeen = f.inFlight
	}
	f.calls = append(f.calls, dataset)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.lists[dataset], nil
}

func (f *fakeLister) seenCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLister) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxSeen
}

// fakeWaker records Wake calls.
type fakeWaker struct{ wakes int }

func (w *fakeWaker) Wake() { w.wakes++ }

func testVM(domain string, sources ...string) model.VM {
	vm := model.VM{Domain: domain}
	for i, src := range sources {
		vm.Disks = append(vm.Disks, model.Disk{Dev: string(rune('a' + i)), Source: src})
	}
	return vm
}

// testService builds a service over canned VMs with tight durations so
// tests never wait on real seconds.
func testService(t *testing.T, vms []model.VM, cfg Config) *Service {
	t.Helper()
	if cfg.VMs == nil {
		cfg.VMs = func() []model.VM { return vms }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Budget == 0 {
		cfg.Budget = 2 * time.Second
	}
	if cfg.Floor == 0 {
		cfg.Floor = 50 * time.Millisecond
	}
	if cfg.Grace == 0 {
		cfg.Grace = 50 * time.Millisecond
	}
	return New(cfg)
}

// waitSettled polls one domain's state until its fetch ends, or fails
// the test after a generous wall clock.
func waitSettled(t *testing.T, s *Service, domain string) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Request(domain, false)
		if !st.Fetching {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fetch for %s never settled", domain)
	return State{}
}

func TestFetchMapsDatasetsAndSortsNewestFirst(t *testing.T) {
	fl := &fakeLister{
		st: truenas.Status{Connected: true, Version: "TrueNAS-25.10.5"},
		lists: map[string][]truenas.Snapshot{
			"nvme/vms/alpine_test-bxuwle": {
				{ID: "d@old", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "old", Created: 100},
				{ID: "d@new", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "new", Created: 200},
			},
		},
	}
	s := testService(t, []model.VM{
		testVM("14_alpine_test", "/dev/zvol/nvme/vms/alpine_test-bxuwle", "/mnt/pool/disks/file.qcow2"),
	}, Config{Lister: fl})

	if got := s.Request("14_alpine_test", true).Fetching; !got {
		t.Fatal("force request should start a fetch")
	}
	st := waitSettled(t, s, "14_alpine_test")

	if len(st.Snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2 (the file-backed disk must be skipped)", len(st.Snapshots))
	}
	if st.Snapshots[0].Name != "new" || st.Snapshots[1].Name != "old" {
		t.Errorf("not newest-first: %s then %s", st.Snapshots[0].Name, st.Snapshots[1].Name)
	}
	if !st.Status.Connected || st.Status.Version != "TrueNAS-25.10.5" || st.Error != "" || st.Stale {
		t.Errorf("state = %+v", st)
	}
	// Exactly one middleware call: the zvol only.
	if calls := fl.seenCalls(); len(calls) != 1 || calls[0] != "nvme/vms/alpine_test-bxuwle" {
		t.Errorf("middleware calls = %v", calls)
	}
}

func TestFailureCarriesOverAndMarksStale(t *testing.T) {
	up := &fakeLister{
		st: truenas.Status{Connected: true},
		lists: map[string][]truenas.Snapshot{
			"nvme/vms/x": {{ID: "nvme/vms/x@yesterday", Dataset: "nvme/vms/x", Name: "yesterday", Created: 50}},
		},
	}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: up})

	s.Request("14_alpine_test", true)
	waitSettled(t, s, "14_alpine_test")

	// The middleware goes down; the held list stays, flagged stale, and
	// the error says the real reason rather than the generic one.
	down := &fakeLister{st: truenas.Status{Connected: false, Error: "dial 192.168.1.100:443: connection refused"}}
	s.cfg.Lister = down
	s.cfg.Waker = &fakeWaker{}

	// Wait out the refetch floor so the force really starts a fetch.
	time.Sleep(60 * time.Millisecond)
	s.Request("14_alpine_test", true)
	st := waitSettled(t, s, "14_alpine_test")

	if len(st.Snapshots) != 1 || st.Snapshots[0].ID != "nvme/vms/x@yesterday" {
		t.Errorf("carry-over failed: %+v", st.Snapshots)
	}
	if !st.Stale {
		t.Error("carried-over list must be marked stale")
	}
	if st.Error != "dial 192.168.1.100:443: connection refused" {
		t.Errorf("error = %q, want the connection's own reason", st.Error)
	}
}

func TestEmptyWithErrorStaysEmptyButSaysWhy(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: true}, err: errors.New("dataset busy")}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl})

	s.Request("14_alpine_test", true)
	st := waitSettled(t, s, "14_alpine_test")

	if len(st.Snapshots) != 0 {
		t.Errorf("snapshots = %+v, want none", st.Snapshots)
	}
	if st.Stale || st.Error != "dataset busy" {
		t.Errorf("state = %+v, want the error and no stale flag (nothing was held)", st)
	}
}

func TestNoZvolDisksNeedsNoMiddleware(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: true}}
	s := testService(t, []model.VM{testVM("9_files", "/mnt/pool/disks/a.qcow2")}, Config{Lister: fl})

	s.Request("9_files", true)
	st := waitSettled(t, s, "9_files")

	if st.Fetching || st.Error != "" || st.Stale {
		t.Errorf("state = %+v, want a clean settled state", st)
	}
	if len(st.Snapshots) != 0 {
		t.Errorf("snapshots = %+v, want an empty list", st.Snapshots)
	}
	if calls := fl.seenCalls(); len(calls) != 0 {
		t.Errorf("middleware called %v for a VM with no zvols", calls)
	}
}

func TestNonForceRequestNeverFetches(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: true}}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl})

	for i := 0; i < 5; i++ {
		if st := s.Request("14_alpine_test", false); st.Fetching {
			t.Fatalf("non-force request started a fetch")
		}
	}
	if calls := fl.seenCalls(); len(calls) != 0 {
		t.Errorf("middleware called %v without any force request", calls)
	}
}

func TestForceRespectsRefetchFloor(t *testing.T) {
	fl := &fakeLister{
		st:    truenas.Status{Connected: true},
		lists: map[string][]truenas.Snapshot{"nvme/vms/x": {{ID: "nvme/vms/x@a", Created: 1}}},
	}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl, Floor: time.Hour})

	s.Request("14_alpine_test", true)
	waitSettled(t, s, "14_alpine_test")

	// A clean fetch just finished: a force inside the floor serves the
	// held copy instead of hitting the middleware again.
	if st := s.Request("14_alpine_test", true); st.Fetching {
		t.Error("force inside the refetch floor should serve the held copy")
	}
	if calls := fl.seenCalls(); len(calls) != 1 {
		t.Errorf("middleware calls = %v, want one", calls)
	}
}

func TestForceAfterErrorRefetchesInsideFloor(t *testing.T) {
	bad := &fakeLister{st: truenas.Status{Connected: true}, err: errors.New("boom")}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: bad, Floor: time.Hour})

	s.Request("14_alpine_test", true)
	waitSettled(t, s, "14_alpine_test")

	// The last attempt failed; a force must try again regardless of the
	// floor, or a recovery could never be seen.
	if st := s.Request("14_alpine_test", true); !st.Fetching {
		t.Error("force after a failed fetch should start a new one")
	}
}

func TestRefreshAllSweepsOneVMAtATime(t *testing.T) {
	fl := &fakeLister{
		st: truenas.Status{Connected: true},
		lists: map[string][]truenas.Snapshot{
			"nvme/vms/a": {{ID: "nvme/vms/x@a", Created: 1}},
			"nvme/vms/b": {{ID: "nvme/vms/x@b", Created: 2}},
		},
		delay: 5 * time.Millisecond,
	}
	vms := []model.VM{
		testVM("1_a", "/dev/zvol/nvme/vms/a"),
		testVM("2_b", "/dev/zvol/nvme/vms/b"),
		testVM("3_c", "/dev/zvol/nvme/vms/c"),
	}
	s := testService(t, vms, Config{Lister: fl})

	if n := s.RefreshAll(); n != 3 {
		t.Fatalf("RefreshAll queued %d, want 3", n)
	}
	for _, vm := range vms {
		waitSettled(t, s, vm.Domain)
	}
	if got := fl.maxConcurrent(); got != 1 {
		t.Errorf("middleware saw %d concurrent queries, want at most 1", got)
	}
	// Every VM state settled clean, including the one with no lists.
	for _, vm := range vms {
		if st := s.Request(vm.Domain, false); st.Fetching || st.Error != "" {
			t.Errorf("state for %s = %+v", vm.Domain, st)
		}
	}
}

func TestRefreshAllDropsVanishedVMsAndSkipsInFlight(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: true}}
	s := testService(t, []model.VM{testVM("1_a", "/dev/zvol/nvme/vms/a")}, Config{Lister: fl})

	s.Request("1_a", true)
	waitSettled(t, s, "1_a")
	s.mu.Lock()
	s.states["9_ghost"] = &state{}
	s.mu.Unlock()

	// 9_ghost is no longer in the poller's list: its state must go. The
	// sweep skips 1_a only while its fetch is inside the refetch floor,
	// so wait it out first.
	time.Sleep(60 * time.Millisecond)
	if n := s.RefreshAll(); n != 1 {
		t.Fatalf("RefreshAll queued %d, want 1", n)
	}
	waitSettled(t, s, "1_a")
	s.mu.Lock()
	_, ghostLeft := s.states["9_ghost"]
	s.mu.Unlock()
	if ghostLeft {
		t.Error("state for a VM the poller no longer reports must be dropped")
	}

	// A sweep during a sweep queues nothing new — and a VM fetched
	// cleanly within the refetch floor is skipped too.
	if n := s.RefreshAll(); n != 0 {
		t.Errorf("second immediate sweep queued %d (fetched within the floor), want 0", n)
	}

	// Once the floor has passed, the sweep picks the VM up again.
	time.Sleep(60 * time.Millisecond)
	if n := s.RefreshAll(); n != 1 {
		t.Errorf("sweep after the floor queued %d, want 1", n)
	}
	waitSettled(t, s, "1_a")
}

func TestWakeFiresWhenDisconnected(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: false, Error: "dial refused"}}
	w := &fakeWaker{}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl, Waker: w, Grace: 10 * time.Millisecond})

	s.Request("14_alpine_test", true)
	waitSettled(t, s, "14_alpine_test")

	if w.wakes == 0 {
		t.Error("a fetch against a down middleware should wake the client")
	}
}

func TestValidateKnown(t *testing.T) {
	fl := &fakeLister{
		st:    truenas.Status{Connected: true},
		lists: map[string][]truenas.Snapshot{"nvme/vms/x": {{ID: "nvme/vms/x@keep", Created: 1}}},
	}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl})
	ctx := context.Background()

	// Nothing held yet: the check fetches and answers from the result.
	ok, err := s.ValidateKnown(ctx, "14_alpine_test", "nvme/vms/x@keep")
	if err != nil || !ok {
		t.Errorf("ValidateKnown known id = %v, %v", ok, err)
	}
	ok, err = s.ValidateKnown(ctx, "14_alpine_test", "nvme/vms/x@ghost")
	if err != nil || ok {
		t.Errorf("ValidateKnown ghost id = %v, %v, want false with no error", ok, err)
	}

	// A failed fetch is an error, never a silent "unknown" — the caller
	// refuses either way, but the difference matters in the message.
	// A fresh domain forces the check down the fetch path (a clean held
	// list answers instantly, by design).
	broken := &fakeLister{st: truenas.Status{Connected: true}, err: errors.New("middleware exploded")}
	s.cfg.Lister = broken
	if _, err := s.ValidateKnown(ctx, "8_broken", "nvme/vms/x@keep"); err == nil {
		t.Error("ValidateKnown against a failing middleware should error")
	}
}

func TestValidateKnownRespectsContext(t *testing.T) {
	fl := &fakeLister{
		st:    truenas.Status{Connected: true},
		lists: map[string][]truenas.Snapshot{"nvme/vms/x": nil},
		delay: time.Second,
	}
	s := testService(t, []model.VM{testVM("14_alpine_test", "/dev/zvol/nvme/vms/x")}, Config{Lister: fl})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.ValidateKnown(ctx, "14_alpine_test", "nvme/vms/x@keep"); err == nil {
		t.Error("ValidateKnown should give up when its context does")
	}
}

func TestBudgetCutsAStuckFetchOff(t *testing.T) {
	slow := &fakeLister{st: truenas.Status{Connected: true}, delay: time.Second}
	s := testService(t, []model.VM{
		testVM("14_alpine_test", "/dev/zvol/nvme/vms/a", "/dev/zvol/nvme/vms/b"),
	}, Config{Lister: slow, Budget: 30 * time.Millisecond})

	s.Request("14_alpine_test", true)
	st := waitSettled(t, s, "14_alpine_test")

	if st.Error == "" {
		t.Error("the budget overrun should be recorded in the state error")
	}
}
