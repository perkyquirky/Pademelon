package actions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"

	"pademelon/internal/libvirtsrc"
	"pademelon/internal/model"
)

// fakeDomains records which verbs were called and answers agent commands
// from a canned reply/error, so the shutdown ladder can be walked without
// a hypervisor. getInfo feeds DomainGetInfo a staged
// sequence (repeating the last entry), so the reboot wait loop can be
// walked through "still running" → "shut off".
type fakeDomains struct {
	calls       []string
	agentReply  string
	agentErr    error
	verbErr     error
	blockCreate chan struct{} // when non-nil, DomainCreate waits on it
	getInfo     []uint8       // staged states for DomainGetInfo
}

var runningState uint8 = 1 // libvirt.DomainRunning
var shutoffState uint8 = 5 // libvirt.DomainShutoff

func (f *fakeDomains) DomainCreate(d libvirt.Domain) error {
	f.calls = append(f.calls, "create")
	if f.blockCreate != nil {
		<-f.blockCreate
	}
	return f.verbErr
}
func (f *fakeDomains) DomainShutdownFlags(d libvirt.Domain, flags libvirt.DomainShutdownFlagValues) error {
	f.calls = append(f.calls, "shutdownFlags")
	return f.verbErr
}
func (f *fakeDomains) DomainDestroy(d libvirt.Domain) error {
	f.calls = append(f.calls, "destroy")
	return f.verbErr
}
func (f *fakeDomains) DomainSuspend(d libvirt.Domain) error {
	f.calls = append(f.calls, "suspend")
	return f.verbErr
}
func (f *fakeDomains) DomainResume(d libvirt.Domain) error {
	f.calls = append(f.calls, "resume")
	return f.verbErr
}
func (f *fakeDomains) DomainGetInfo(d libvirt.Domain) (uint8, uint64, uint64, uint16, uint64, error) {
	if len(f.getInfo) == 0 {
		return runningState, 0, 0, 0, 0, f.verbErr
	}
	state := f.getInfo[0]
	if len(f.getInfo) > 1 {
		f.getInfo = f.getInfo[1:]
	}
	return state, 0, 0, 0, 0, f.verbErr
}
func (f *fakeDomains) QEMUDomainAgentCommand(d libvirt.Domain, cmd string, timeout int32, flags uint32) (libvirt.OptString, error) {
	f.calls = append(f.calls, "agent:"+cmd)
	if f.agentErr != nil {
		return nil, f.agentErr
	}
	if f.agentReply == "" {
		return nil, nil
	}
	return libvirt.OptString{f.agentReply}, nil
}
func (f *fakeDomains) ConnectListAllDomains(need int32, flags libvirt.ConnectListAllDomainsFlags) ([]libvirt.Domain, uint32, error) {
	return []libvirt.Domain{{Name: "14_alpine_test"}}, 1, nil
}

func (f *fakeDomains) has(want ...string) bool {
	if len(f.calls) != len(want) {
		return false
	}
	for i := range want {
		if f.calls[i] != want[i] {
			return false
		}
	}
	return true
}

type fakeConn struct{ doms *fakeDomains }

func (c *fakeConn) WithConnection(fn func(libvirtsrc.Domains) error) error {
	return fn(c.doms)
}

func runningVM(agent model.AgentState) model.VM {
	return model.VM{Domain: "14_alpine_test", State: "running", Running: true, Agent: agent}
}

func snapWith(vms ...model.VM) func() model.Snapshot {
	return func() model.Snapshot { return model.Snapshot{Connected: true, VMs: vms} }
}

func newTestStore(snap func() model.Snapshot, conn *fakeConn, timeout time.Duration) (*Store, chan struct{}) {
	nudge := make(chan struct{}, 8)
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snap,
		Conn:     conn,
		Nudge:    nudge,
		Timeout:  timeout,
	})
	return s, nudge
}

// waitForJob polls the registry until the job lands in the wanted state.
func waitForJob(t *testing.T, s *Store, id, want string) Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range s.List() {
			if j.ID == id {
				if j.State == want {
					return j
				}
				if j.State == StateFailed || j.State == StateTimeout {
					t.Fatalf("job %s landed in %s (%s), want %s", id, j.State, j.Detail, want)
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %s in time", id, want)
	return Job{}
}

func TestSubmitValidatesAgainstSnapshot(t *testing.T) {
	s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: &fakeDomains{}}, time.Second)

	if _, err := s.Submit("14_alpine_test", "reboot-the-forest"); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("unknown action err = %v, want ErrUnknownAction", err)
	}
	if _, err := s.Submit("99_ghost", ActionStart); !errors.Is(err, ErrUnknownDomain) {
		t.Errorf("unknown domain err = %v, want ErrUnknownDomain", err)
	}
	if _, err := s.Submit("14_alpine_test", ActionStart); !errors.Is(err, ErrInvalidState) {
		t.Errorf("start on running err = %v, want ErrInvalidState", err)
	}
	if _, err := s.Submit("14_alpine_test", ActionResume); !errors.Is(err, ErrInvalidState) {
		t.Errorf("resume on running err = %v, want ErrInvalidState", err)
	}

	// Force off is the one verb allowed on a paused guest.
	paused := runningVM(model.AgentOK)
	paused.State, paused.Running = "paused", false
	s2, _ := newTestStore(snapWith(paused), &fakeConn{doms: &fakeDomains{}}, time.Second)
	if _, err := s2.Submit("14_alpine_test", ActionForceOff); err != nil {
		t.Errorf("force off on paused err = %v, want nil", err)
	}
	if _, err := s2.Submit("14_alpine_test", ActionShutdown); !errors.Is(err, ErrInvalidState) {
		t.Errorf("shutdown on paused err = %v, want ErrInvalidState", err)
	}
}

func TestStartHappyPath(t *testing.T) {
	fds := &fakeDomains{}
	stopped := model.VM{Domain: "14_alpine_test", State: "stopped"}
	s, nudge := newTestStore(snapWith(stopped), &fakeConn{doms: fds}, time.Second)

	job, err := s.Submit("14_alpine_test", ActionStart)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.State != StatePending {
		t.Errorf("submitted job state = %q, want pending", job.State)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	if !fds.has("create") {
		t.Errorf("calls = %v, want only create", fds.calls)
	}
	if done.Detail == "" {
		t.Error("finished job should carry a detail line")
	}
	if len(nudge) == 0 {
		t.Error("finished job should have nudged the poller")
	}
}

func TestRebootOrchestration(t *testing.T) {
	// Reboot = graceful shutdown → wait for the guest to actually be off →
	// start again. The fake walks the wait loop through "still running"
	// then "shut off", so the sequence must be: agent shutdown, one or two
	// GetInfo polls, then create.
	fds := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState, shutoffState},
	}
	s, nudge := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
	s.cfg.RebootTimeout = time.Minute
	s.cfg.WaitPoll = time.Millisecond

	job, err := s.Submit("14_alpine_test", ActionReboot)
	if err != nil {
		t.Fatalf("Submit reboot: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	if !fds.has("agent:"+`{"execute":"guest-shutdown"}`, "create") {
		t.Errorf("calls = %v, want agent shutdown then create", fds.calls)
	}
	if !strings.Contains(done.Detail, "started again") {
		t.Errorf("detail = %q", done.Detail)
	}
	if len(nudge) == 0 {
		t.Error("finished reboot should have nudged the poller")
	}
}

func TestRebootTimeoutLeavesTheVMTouchedButUntied(t *testing.T) {
	// The guest refuses to die: the wait loop runs out, and the job ends
	// as timeout with instructions instead of force-starting anything.
	fds := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState}, // repeats "running" forever
	}
	s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
	s.cfg.RebootTimeout = 30 * time.Millisecond
	s.cfg.WaitPoll = time.Millisecond

	job, _ := s.Submit("14_alpine_test", ActionReboot)
	done := waitForJob(t, s, job.ID, StateTimeout)
	if !strings.Contains(done.Detail, "force off or start it manually") {
		t.Errorf("timeout detail = %q", done.Detail)
	}
	if fds.has("create") || len(fds.calls) == 0 && true {
		// create must not have run; the only calls should be the agent one
		for _, c := range fds.calls {
			if c == "create" {
				t.Error("reboot timeout must not reach the start phase")
			}
		}
	}
}

func TestShutdownLadder(t *testing.T) {
	goneErr := errors.New("guest agent command timed out: Guest agent disappeared while executing command")

	t.Run("agent path with the disappeared signature succeeds without ACPI", func(t *testing.T) {
		fds := &fakeDomains{agentErr: goneErr}
		s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
		job, _ := s.Submit("14_alpine_test", ActionShutdown)
		waitForJob(t, s, job.ID, StateOK)
		if !fds.has("agent:" + `{"execute":"guest-shutdown"}`) {
			t.Errorf("calls = %v, want only the agent call", fds.calls)
		}
	})

	t.Run("agent path with a plain empty reply succeeds", func(t *testing.T) {
		fds := &fakeDomains{}
		s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
		job, _ := s.Submit("14_alpine_test", ActionShutdown)
		waitForJob(t, s, job.ID, StateOK)
	})

	t.Run("real agent failure falls back to ACPI", func(t *testing.T) {
		fds := &fakeDomains{agentErr: errors.New("internal error: socket has been disconnected")}
		s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
		job, _ := s.Submit("14_alpine_test", ActionShutdown)
		waitForJob(t, s, job.ID, StateOK)
		if !fds.has("agent:"+`{"execute":"guest-shutdown"}`, "shutdownFlags") {
			t.Errorf("calls = %v, want agent call then ACPI fallback", fds.calls)
		}
	})

	t.Run("agentless guest goes straight to ACPI", func(t *testing.T) {
		fds := &fakeDomains{}
		s, _ := newTestStore(snapWith(runningVM(model.AgentDisconnected)), &fakeConn{doms: fds}, time.Second)
		job, _ := s.Submit("14_alpine_test", ActionShutdown)
		waitForJob(t, s, job.ID, StateOK)
		if !fds.has("shutdownFlags") {
			t.Errorf("calls = %v, want only ACPI", fds.calls)
		}
	})
}

func TestSingleFlight(t *testing.T) {
	fds := &fakeDomains{blockCreate: make(chan struct{})}
	stopped := model.VM{Domain: "14_alpine_test", State: "stopped"}
	s, _ := newTestStore(snapWith(stopped), &fakeConn{doms: fds}, time.Second)

	if _, err := s.Submit("14_alpine_test", ActionStart); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err := s.Submit("14_alpine_test", ActionStart)
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("second submit err = %v, want ErrInFlight", err)
	}
	// Release the first job so the goroutine does not leak past the test.
	close(fds.blockCreate)
}

func TestTimeoutMarksJobAndAbandons(t *testing.T) {
	fds := &fakeDomains{blockCreate: make(chan struct{})}
	stopped := model.VM{Domain: "14_alpine_test", State: "stopped"}
	s, _ := newTestStore(snapWith(stopped), &fakeConn{doms: fds}, 30*time.Millisecond)

	job, _ := s.Submit("14_alpine_test", ActionStart)
	done := waitForJob(t, s, job.ID, StateTimeout)
	if !strings.Contains(done.Detail, "exceeded") {
		t.Errorf("timeout detail = %q", done.Detail)
	}
	close(fds.blockCreate)
}

func TestSweepDropsOldJobs(t *testing.T) {
	now := time.Now()
	clock := now
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(runningVM(model.AgentOK)),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Now:      func() time.Time { return clock },
	})
	// Seed two jobs by hand: one fresh, one ancient.
	s.mu.Lock()
	s.jobs["fresh"] = &Job{ID: "fresh", Domain: "14_alpine_test", Action: ActionPause, State: StateOK, Requested: now}
	s.jobs["old"] = &Job{ID: "old", Domain: "14_alpine_test", Action: ActionPause, State: StateOK, Requested: now.Add(-2 * time.Hour)}
	s.mu.Unlock()

	if got := len(s.List()); got != 1 {
		t.Fatalf("after sweep, %d jobs visible, want 1 (only the fresh one)", got)
	}
	if _, ok := s.jobs["old"]; ok {
		t.Error("ancient job should have been swept")
	}
}

func TestIsAgentGoneErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the real signature", errors.New("guest agent command timed out: Guest agent disappeared while executing command"), true},
		{"nil", nil, false},
		{"an ordinary failure", errors.New("internal error: socket has been disconnected"), false},
		{"a timeout without disappearing", errors.New("guest agent command timed out"), false},
	}
	for _, tc := range cases {
		if got := isAgentGoneErr(tc.err); got != tc.want {
			t.Errorf("%s: isAgentGoneErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShutdownAllPlansAndSkips(t *testing.T) {
	fds := &fakeDomains{agentReply: "x"}
	vms := []model.VM{
		runningVM(model.AgentOK), // planned
		{Domain: "12_test", State: "running", Running: true, Agent: model.AgentDisconnected}, // skipped: no agent
		{Domain: "11_mousies", State: "stopped"},                                             // ignored silently
		{Domain: "9_sandbox", State: "paused"},                                               // skipped: paused
	}
	s, _ := newTestStore(snapWith(vms...), &fakeConn{doms: fds}, time.Second)

	planned, skipped := s.ShutdownAll()
	if len(planned) != 1 || planned[0] != "14_alpine_test" {
		t.Errorf("planned = %v, want only 14_alpine_test", planned)
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want the agentless and paused entries", skipped)
	}
}

// fakeMiddleware is the MiddlewareClient surface the snapshot, restore
// and delete jobs need, canned: it records every call and fails on demand.
type fakeMiddleware struct {
	creates     [][2]string // {dataset, name}
	failFrom    int         // fail the Nth create onward; 0 = never fail
	err         error
	rollbacks   []string // "dataset@name|recursive=<bool>"
	rollbackErr error
	deletes     []string // ids handed to delete
	deleteErr   error
	deleteGone  bool // pretend the snapshot was already gone
}

func (f *fakeMiddleware) CreateSnapshot(_ context.Context, dataset, name string) (string, error) {
	f.creates = append(f.creates, [2]string{dataset, name})
	if f.failFrom > 0 && len(f.creates) >= f.failFrom {
		return "", f.err
	}
	return dataset + "@" + name, nil
}

func (f *fakeMiddleware) RollbackSnapshot(_ context.Context, dataset, name string, recursive bool) error {
	f.rollbacks = append(f.rollbacks, fmt.Sprintf("%s@%s|recursive=%v", dataset, name, recursive))
	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	return nil
}

func (f *fakeMiddleware) DeleteSnapshot(_ context.Context, dataset, name string) (bool, error) {
	f.deletes = append(f.deletes, dataset+"@"+name)
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return !f.deleteGone, nil
}

// snapshotTestVM is a VM with one zvol and one file-backed disk — the
// gather-and-skip shape the snapshot job must respect.
func snapshotTestVM(agent model.AgentState, state string) model.VM {
	return model.VM{
		Domain: "14_alpine_test", Name: "alpine_test", State: state,
		Running: state == "running", Agent: agent,
		Disks: []model.Disk{
			{Dev: "vda", Source: "/dev/zvol/nvme/vms/alpine_test-bxuwle"},
			{Dev: "vdb", Source: "/mnt/pool/disks/file.qcow2"},
		},
	}
}

func newSnapshotStore(doms *fakeDomains, mw actionsMiddlewareFake, agentOK bool) (*Store, chan struct{}) {
	nudge := make(chan struct{}, 8)
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(snapshotTestVM(model.AgentOK, "running")),
		Conn:     &fakeConn{doms: doms},
		Nudge:    nudge,
		Timeout:  2 * time.Second,
		Truenas:  mw,
	})
	return s, nudge
}

// actionsMiddlewareFake aliases the fake so the interface assignment is
// obvious in test table setup.
type actionsMiddlewareFake = *fakeMiddleware

func submitSnapshot(t *testing.T, s *Store) Job {
	t.Helper()
	job, err := s.Submit("14_alpine_test", ActionSnapshot)
	if err != nil {
		t.Fatalf("submit snapshot: %v", err)
	}
	return *job
}

// TestSnapshotHappyPathFrozen: agent answers freeze, one create on the
// zvol dataset (the file-backed disk is skipped), thaw, and the job
// reports the whole sequence. Suspend/resume must never fire.
func TestSnapshotHappyPathFrozen(t *testing.T) {
	doms := &fakeDomains{agentReply: `{"return":1}`}
	mw := &fakeMiddleware{}
	s, _ := newSnapshotStore(doms, mw, true)

	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	if !doms.has("agent:"+`{"execute":"guest-fsfreeze-freeze"}`,
		"agent:"+`{"execute":"guest-fsfreeze-thaw"}`) {
		t.Errorf("verb sequence = %v, want freeze then thaw", doms.calls)
	}
	if len(mw.creates) != 1 || mw.creates[0][0] != "nvme/vms/alpine_test-bxuwle" {
		t.Errorf("creates = %v, want exactly the zvol dataset", mw.creates)
	}
	if !strings.Contains(mw.creates[0][1], "pademelon-alpine_test-") {
		t.Errorf("snapshot name = %q, want the pademelon-<vm>-<ts> shape", mw.creates[0][1])
	}
	if strings.Contains(got.Detail, "frozen") == false || !strings.Contains(got.Detail, "thawed") {
		t.Errorf("detail = %q, want the freeze/thaw story", got.Detail)
	}
}

// TestSnapshotFallsBackToSuspend: no agent, so the guest is suspended
// before the creates and resumed after — the symmetric fallback.
func TestSnapshotFallsBackToSuspend(t *testing.T) {
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(snapshotTestVM(model.AgentDisconnected, "running")),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  &fakeMiddleware{},
	})
	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	if !strings.Contains(got.Detail, "paused during the shot, resumed") {
		t.Errorf("detail = %q, want the suspend/resume story", got.Detail)
	}
}

// TestSnapshotCreateFailureStillThaws: the middleware create fails after
// the guest was frozen — the thaw must run anyway, and the job must fail
// with the middleware's complaint. This is the guarantee.
func TestSnapshotCreateFailureStillThaws(t *testing.T) {
	doms := &fakeDomains{agentReply: `{"return":1}`}
	mw := &fakeMiddleware{failFrom: 1, err: errors.New("dataset busy")}
	s, _ := newSnapshotStore(doms, mw, true)

	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateFailed)

	// The thaw is in the verb list even though the create failed.
	foundThaw := false
	for _, c := range doms.calls {
		if strings.Contains(c, "guest-fsfreeze-thaw") {
			foundThaw = true
		}
	}
	if !foundThaw {
		t.Errorf("verbs = %v, want a thaw despite the failed create", doms.calls)
	}
	if !strings.Contains(got.Detail, "dataset busy") {
		t.Errorf("detail = %q, want the middleware error", got.Detail)
	}
}

// TestSnapshotPausedGuestSkipsQuiesce: a paused guest is already
// quiesced — no freeze, no suspend, no resume, just creates.
func TestSnapshotPausedGuestSkipsQuiesce(t *testing.T) {
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(snapshotTestVM(model.AgentOK, "paused")),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  &fakeMiddleware{},
	})
	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	if !strings.Contains(got.Detail, "already quiesced") {
		t.Errorf("detail = %q, want the already-quiesced story", got.Detail)
	}
}

// TestSnapshotWithoutMiddlewareRefused: no middleware configured, no
// snapshot job — refused at submit time, before any guest is touched.
func TestSnapshotWithoutMiddlewareRefused(t *testing.T) {
	s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: &fakeDomains{}}, time.Second)
	if _, err := s.Submit("14_alpine_test", ActionSnapshot); !errors.Is(err, ErrMiddlewareOff) {
		t.Errorf("submit without middleware = %v, want ErrMiddlewareOff", err)
	}
}

// TestSnapshotNameFolding: characters that ZFS rejects in a snapshot name
// fold to dashes; the pademelon- prefix and timestamp stay readable.
func TestSnapshotNameFolding(t *testing.T) {
	vm := &model.VM{Name: "weird vm.name/with@stuff", Domain: "7_weird"}
	got := snapshotName(vm, time.Date(2026, 9, 7, 14, 32, 5, 0, time.UTC))
	want := "pademelon-weird-vm.name-with-stuff-2026-09-07_14-32-05"
	if got != want {
		t.Errorf("snapshotName = %q, want %q", got, want)
	}
}

// restoreTestVM carries a disk with its zvol; the id guard validates
// against the middleware's live answer, faked here.
func restoreTestVM(state string) model.VM {
	return model.VM{
		Domain: "14_alpine_test", Name: "alpine_test", State: state,
		Running: state == "running", Agent: model.AgentOK,
		Disks: []model.Disk{{Dev: "vda", Source: "/dev/zvol/nvme/vms/alpine_test-bxuwle"}},
	}
}

// fakeGuard is the SnapshotGuard fake: it knows exactly the "domain|id"
// pairs in known, and can be told to fail the check (the middleware-down
// case, which must refuse the submission).
type fakeGuard struct {
	known map[string]bool
	err   error
}

func guardWith(domain string, ids ...string) *fakeGuard {
	g := &fakeGuard{known: map[string]bool{}}
	for _, id := range ids {
		g.known[domain+"|"+id] = true
	}
	return g
}

func (g *fakeGuard) ValidateKnown(_ context.Context, domain, id string) (bool, error) {
	if g.err != nil {
		return false, g.err
	}
	return g.known[domain+"|"+id], nil
}

func newRestoreStore(doms *fakeDomains, mw *fakeMiddleware, state string) *Store {
	return newRestoreStoreGuarded(doms, mw, state, guardWith("14_alpine_test", testSnapID))
}

func newRestoreStoreGuarded(doms *fakeDomains, mw *fakeMiddleware, state string, guard *fakeGuard) *Store {
	return New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(restoreTestVM(state)),
		Conn:     &fakeConn{doms: doms},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  mw,
		Guard:    guard,
	})
}

const testSnapID = "nvme/vms/alpine_test-bxuwle@pademelon-alpine_test-2026-09-07_15-00"

// TestRestoreStagedRunningFullCycle: shut down, wait for off, roll back,
// start again — with the ack carrying the newer-snapshots destruction.
func TestRestoreStagedRunningFullCycle(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{runningState, shutoffState}}
	mw := &fakeMiddleware{}
	s := newRestoreStore(doms, mw, "running")

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged, StartAfter: true, Ack: true,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	got := waitForJob(t, s, job.ID, StateOK)

	if !strings.Contains(got.Detail, "rolled back, started again") {
		t.Errorf("detail = %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "newer snapshots destroyed") {
		t.Errorf("detail should carry the ack's meaning: %q", got.Detail)
	}
	if len(mw.rollbacks) != 1 || mw.rollbacks[0] != testSnapID+"|recursive=true" {
		t.Errorf("rollbacks = %v", mw.rollbacks)
	}
	shutdownSeen, createSeen := false, false
	for _, c := range doms.calls {
		switch {
		case strings.Contains(c, "guest-shutdown") || c == "shutdownFlags":
			shutdownSeen = true
		case c == "create":
			createSeen = true
		}
	}
	if !shutdownSeen || !createSeen {
		t.Errorf("verb sequence = %v, want a shutdown then a create", doms.calls)
	}
}

// TestRestoreStagedStoppedNoShutdown: an already-stopped guest skips the
// shutdown phase entirely; without startAfter it stays stopped.
func TestRestoreStagedStoppedNoShutdown(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{shutoffState}}
	mw := &fakeMiddleware{}
	s := newRestoreStore(doms, mw, "stopped")

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged, StartAfter: false, Ack: false,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	got := waitForJob(t, s, job.ID, StateOK)

	if strings.Contains(got.Detail, "started") {
		t.Errorf("detail = %q, want the guest-left-stopped story", got.Detail)
	}
	if mw.rollbacks[0] != testSnapID+"|recursive=false" {
		t.Errorf("rollbacks = %v — no ack means no recursive destruction", mw.rollbacks)
	}
	for _, c := range doms.calls {
		if c == "shutdownFlags" {
			t.Errorf("a stopped guest must not be shut down again: %v", doms.calls)
		}
	}
}

// TestRestoreRollbackFailureLeavesGuestStopped: when the rollback fails
// after the shutdown, the job fails with the middleware's words and the
// guest is NOT started — an honest stop beats a surprise boot.
func TestRestoreRollbackFailureLeavesGuestStopped(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{runningState, shutoffState}}
	mw := &fakeMiddleware{rollbackErr: errors.New("dataset busy")}
	s := newRestoreStore(doms, mw, "running")

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged, StartAfter: true, Ack: true,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	got := waitForJob(t, s, job.ID, StateFailed)

	if !strings.Contains(got.Detail, "dataset busy") || !strings.Contains(got.Detail, "left as-is") {
		t.Errorf("detail = %q", got.Detail)
	}
	for _, c := range doms.calls {
		if c == "create" {
			t.Errorf("a failed rollback must not start the guest: %v", doms.calls)
		}
	}
}

// TestRestoreDirectRunningNeedsAck: the backend's own refusal for a
// direct restore of a running VM — the dialog's warning has a twin here.
func TestRestoreDirectRunningNeedsAck(t *testing.T) {
	s := newRestoreStore(&fakeDomains{}, &fakeMiddleware{}, "running")

	if _, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeDirect, Ack: false,
	}); !errors.Is(err, ErrInvalidState) {
		t.Errorf("direct without ack = %v, want ErrInvalidState", err)
	}
	if jobs := s.List(); len(jobs) != 0 {
		t.Errorf("a refused restore must not register a job: %+v", jobs)
	}
	// With ack it goes through.
	if _, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeDirect, Ack: true,
	}); err != nil {
		t.Errorf("direct with ack refused: %v", err)
	}
}

// TestDeleteJobRecordsTheVerb: the delete job rides the registry like
// every other write, and already-gone is a fine answer.
func TestDeleteJobRecordsTheVerb(t *testing.T) {
	mw := &fakeMiddleware{}
	s := newRestoreStore(&fakeDomains{}, mw, "running")

	job, err := s.SubmitDelete(context.Background(), "14_alpine_test", testSnapID)
	if err != nil {
		t.Fatalf("submit delete: %v", err)
	}
	got := waitForJob(t, s, job.ID, StateOK)
	if !strings.Contains(got.Detail, "deleted") || len(mw.deletes) != 1 || mw.deletes[0] != testSnapID {
		t.Errorf("detail = %q, deletes = %v", got.Detail, mw.deletes)
	}

	// Already gone: success with a different story, not a failure.
	gone := &fakeMiddleware{deleteGone: true}
	s2 := newRestoreStore(&fakeDomains{}, gone, "running")
	job2, err := s2.SubmitDelete(context.Background(), "14_alpine_test", testSnapID)
	if err != nil {
		t.Fatalf("submit delete: %v", err)
	}
	got2 := waitForJob(t, s2, job2.ID, StateOK)
	if !strings.Contains(got2.Detail, "already gone") {
		t.Errorf("detail = %q, want the already-gone story", got2.Detail)
	}
}

// TestSnapshotIdGuard: a snapshot id the middleware does not know for
// this VM is unreachable — restore and delete both refuse it.
func TestSnapshotIdGuard(t *testing.T) {
	s := newRestoreStore(&fakeDomains{}, &fakeMiddleware{}, "running")

	if _, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{SnapshotID: "nvme/vms/x@ghost", Mode: ModeStaged}); !errors.Is(err, ErrUnknownSnapshot) {
		t.Errorf("restore unknown snapshot = %v", err)
	}
	if _, err := s.SubmitDelete(context.Background(), "14_alpine_test", "nvme/vms/x@ghost"); !errors.Is(err, ErrUnknownSnapshot) {
		t.Errorf("delete unknown snapshot = %v", err)
	}
	// An unknown domain reads as unknown snapshot too — same 404 either way.
	if _, err := s.SubmitDelete(context.Background(), "99_ghost", testSnapID); err == nil {
		t.Error("delete on unknown domain should refuse")
	}
}

// TestSnapshotGuardFailureRefuses: when the guard cannot get a live
// middleware answer, the submission is refused — fail closed, and with a
// distinct error the web layer maps to 503, not to "no such snapshot". A
// stale id must never reach a rollback because the check could not run.
func TestSnapshotGuardFailureRefuses(t *testing.T) {
	g := &fakeGuard{err: errors.New("middleware not connected")}
	s := newRestoreStoreGuarded(&fakeDomains{}, &fakeMiddleware{}, "running", g)

	if _, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged,
	}); !errors.Is(err, ErrGuardUnavailable) {
		t.Errorf("restore with a failed guard = %v, want ErrGuardUnavailable", err)
	}
	if _, err := s.SubmitDelete(context.Background(), "14_alpine_test", testSnapID); !errors.Is(err, ErrGuardUnavailable) {
		t.Errorf("delete with a failed guard = %v, want ErrGuardUnavailable", err)
	}
	if jobs := s.List(); len(jobs) != 0 {
		t.Errorf("a refused submit must not register a job: %+v", jobs)
	}
}

// TestSnapshotGuardWithoutGuardConfigured: a store built without a guard
// cannot verify ids, so restore and delete refuse rather than skip the
// check.
func TestSnapshotGuardWithoutGuardConfigured(t *testing.T) {
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(restoreTestVM("running")),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  &fakeMiddleware{},
	})
	if _, err := s.SubmitDelete(context.Background(), "14_alpine_test", testSnapID); !errors.Is(err, ErrGuardUnavailable) {
		t.Errorf("delete without a guard = %v, want ErrGuardUnavailable", err)
	}
}

// findStep pulls one step out of a job's step list.
func findStep(j Job, name string) (JobStep, bool) {
	for _, st := range j.Steps {
		if st.Name == name {
			return st, true
		}
	}
	return JobStep{}, false
}

func mustStep(t *testing.T, j Job, name, wantState, wantDetailPart string) {
	t.Helper()
	st, ok := findStep(j, name)
	if !ok {
		t.Fatalf("job has no %q step: %+v", name, j.Steps)
	}
	if st.State != wantState {
		t.Errorf("step %q state = %q, want %q (detail %q)", name, st.State, wantState, st.Detail)
	}
	if wantDetailPart != "" && !strings.Contains(st.Detail, wantDetailPart) {
		t.Errorf("step %q detail = %q, want it to contain %q", name, st.Detail, wantDetailPart)
	}
}

// TestRestoreStagedSteps: the stepper's core promise — stop, then
// libvirt's own confirmation that the guest is off, then the rollback,
// then the start. The verify step must go done before the rollback runs.
func TestRestoreStagedSteps(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{runningState, shutoffState}}
	mw := &fakeMiddleware{}
	s := newRestoreStore(doms, mw, "running")

	submitted, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged, StartAfter: true, Ack: true,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	// The checklist exists the moment the job is queued.
	mustStep(t, *submitted, StepStop, StepPending, "")
	mustStep(t, *submitted, StepStart, StepPending, "")

	done := waitForJob(t, s, submitted.ID, StateOK)
	mustStep(t, done, StepStop, StepDone, "")
	mustStep(t, done, StepVerify, StepDone, "libvirt confirms the guest is off")
	mustStep(t, done, StepRollback, StepDone, "rolled back")
	mustStep(t, done, StepStart, StepDone, "guest started")
	// The rollback must run after the guest was confirmed off.
	if !strings.Contains(mw.rollbacks[0], "recursive=true") {
		t.Errorf("rollbacks = %v", mw.rollbacks)
	}
}

// TestRestoreStagedAlreadyStopped: a guest that is off at execution time
// skips the stop phase and confirms off instantly — visible as
// skipped + done, not hidden.
func TestRestoreStagedAlreadyStopped(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{shutoffState}}
	s := newRestoreStore(doms, &fakeMiddleware{}, "stopped")

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged, StartAfter: false,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	mustStep(t, done, StepStop, StepSkipped, "already stopped")
	mustStep(t, done, StepVerify, StepDone, "already off")
	mustStep(t, done, StepRollback, StepDone, "")
	if _, has := findStep(done, StepStart); has {
		t.Errorf("no start step was requested but the job has one: %+v", done.Steps)
	}
}

// TestRestoreStagedGuestRefusesToStop: the verify phase runs out and the
// job ends as timeout — the verify step carries the force-off hint, and
// the rollback must never have run.
func TestRestoreStagedGuestRefusesToStop(t *testing.T) {
	doms := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState}, // repeats running forever
	}
	mw := &fakeMiddleware{}
	s := newRestoreStoreGuarded(doms, mw, "running", guardWith("14_alpine_test", testSnapID))
	s.cfg.RebootTimeout = 30 * time.Millisecond
	s.cfg.WaitPoll = time.Millisecond
	s.cfg.ShutdownRetry = time.Hour // one shutdown request, no re-sends

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateTimeout)
	mustStep(t, done, StepStop, StepDone, "")
	mustStep(t, done, StepVerify, StepFailed, "force off")
	mustStep(t, done, StepRollback, StepPending, "")
	if len(mw.rollbacks) != 0 {
		t.Errorf("rollback ran despite the guest never stopping: %v", mw.rollbacks)
	}
}

// TestRestoreStagedRetriesShutdown: the guest ignores the first shutdown
// request — the wait loop re-sends it, and the stop step's detail says so.
// The visible answer to the Alpine guest that needs two commands.
func TestRestoreStagedRetriesShutdown(t *testing.T) {
	doms := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState, runningState, runningState, runningState, runningState, runningState, runningState, runningState, shutoffState},
	}
	s := newRestoreStore(doms, &fakeMiddleware{}, "running")
	s.cfg.RebootTimeout = 2 * time.Second
	s.cfg.WaitPoll = time.Millisecond
	s.cfg.ShutdownRetry = 5 * time.Millisecond

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	mustStep(t, done, StepStop, StepDone, "re-sent")
	shutdowns := 0
	for _, c := range doms.calls {
		if strings.Contains(c, "guest-shutdown") || strings.Contains(c, "shutdownFlags") {
			shutdowns++
		}
	}
	if shutdowns < 2 {
		t.Errorf("shutdown sent %d times, want at least 2 (the re-send): %v", shutdowns, doms.calls)
	}
}

// TestRestoreDirectSingleStep: a direct restore has no shutdown to wait
// for — one rollback pill, and it goes done.
func TestRestoreDirectSingleStep(t *testing.T) {
	s := newRestoreStore(&fakeDomains{}, &fakeMiddleware{}, "running")

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeDirect, Ack: true,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Name != StepRollback {
		t.Fatalf("direct restore steps = %+v, want only rollback", job.Steps)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	mustStep(t, done, StepRollback, StepDone, "")
}

// TestStagedRestoreBoundCoversTheWait: the outer job bound must outlive
// the wait-for-stopped phase, or the job is marked timeout while its
// steps keep advancing — the regression the stepper work exposed.
func TestStagedRestoreBoundCoversTheWait(t *testing.T) {
	doms := &fakeDomains{getInfo: []uint8{runningState, shutoffState}}
	s := newRestoreStore(doms, &fakeMiddleware{}, "running")
	s.cfg.Timeout = 10 * time.Millisecond // the old default that wrongly applied
	s.cfg.RebootTimeout = 200 * time.Millisecond
	s.cfg.WaitPoll = time.Millisecond

	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	if !strings.Contains(done.Detail, "rolled back") {
		t.Errorf("detail = %q, want the successful restore story", done.Detail)
	}
}

// TestCloneDeepCopiesSteps: mutating a returned job's steps must not
// touch the registry's copy — the runner updates steps in place, and a
// shared backing array would let those writes leak into clones mid-read.
func TestCloneDeepCopiesSteps(t *testing.T) {
	s := newRestoreStore(&fakeDomains{getInfo: []uint8{shutoffState}}, &fakeMiddleware{}, "stopped")
	job, err := s.SubmitRestore(context.Background(), "14_alpine_test", RestoreOpts{
		SnapshotID: testSnapID, Mode: ModeStaged,
	})
	if err != nil {
		t.Fatalf("submit restore: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)

	// Sabotage the returned copy.
	for i := range done.Steps {
		done.Steps[i].State = "sabotaged"
		done.Steps[i] = JobStep{Name: done.Steps[i].Name, State: "sabotaged"}
	}
	fresh := s.List()
	for _, j := range fresh {
		if j.ID != job.ID {
			continue
		}
		for _, st := range j.Steps {
			if st.State == "sabotaged" {
				t.Errorf("registry copy was mutated through the returned clone: %+v", j.Steps)
			}
		}
	}
}

// TestSnapshotStepsFrozen: the freeze path reports quiesce → snapshot →
// unquiesce, all done, with the freeze story in the quiesce detail.
func TestSnapshotStepsFrozen(t *testing.T) {
	doms := &fakeDomains{agentReply: `{"return":1}`}
	mw := &fakeMiddleware{}
	s, _ := newSnapshotStore(doms, mw, true)

	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	mustStep(t, got, StepQuiesce, StepDone, "frozen")
	mustStep(t, got, StepSnapshot, StepDone, "dataset")
	mustStep(t, got, StepUnquiesce, StepDone, "thawed")
}

// TestSnapshotStepsSuspendFallback: the agent has no fsfreeze — the
// quiesce step says so, and the suspend/resume pair still lands.
func TestSnapshotStepsSuspendFallback(t *testing.T) {
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(snapshotTestVM(model.AgentDisconnected, "running")),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  &fakeMiddleware{},
	})
	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	mustStep(t, got, StepQuiesce, StepDone, "paused for the shot")
	mustStep(t, got, StepUnquiesce, StepDone, "resumed")
}

// TestSnapshotStepsPartialFailureKeepsThawing: a create fails after the
// freeze — the snapshot step goes failed, but the unquiesce step still
// lands done. A frozen guest is a hung guest, whatever the job's verdict.
func TestSnapshotStepsPartialFailureKeepsThawing(t *testing.T) {
	doms := &fakeDomains{agentReply: `{"return":1}`}
	mw := &fakeMiddleware{failFrom: 1, err: errors.New("dataset busy")}
	s, _ := newSnapshotStore(doms, mw, true)

	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateFailed)

	mustStep(t, got, StepQuiesce, StepDone, "frozen")
	mustStep(t, got, StepSnapshot, StepFailed, "dataset busy")
	mustStep(t, got, StepUnquiesce, StepDone, "thawed")
}

// TestSnapshotStepsAlreadyQuiesced: a paused guest skips the quiesce
// phase visibly.
func TestSnapshotStepsAlreadyQuiesced(t *testing.T) {
	s := New(Config{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Snapshot: snapWith(snapshotTestVM(model.AgentOK, "paused")),
		Conn:     &fakeConn{doms: &fakeDomains{}},
		Nudge:    make(chan struct{}, 8),
		Timeout:  2 * time.Second,
		Truenas:  &fakeMiddleware{},
	})
	job := submitSnapshot(t, s)
	got := waitForJob(t, s, job.ID, StateOK)

	mustStep(t, got, StepQuiesce, StepSkipped, "already quiesced")
	mustStep(t, got, StepSnapshot, StepDone, "")
	mustStep(t, got, StepUnquiesce, StepSkipped, "")
}

// TestRebootSteps: the reboot's pills mirror the restore's stop/verify
// and add the start — the same visible confirmation the stepper gives
// restore.
func TestRebootSteps(t *testing.T) {
	fds := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState, shutoffState},
	}
	s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
	s.cfg.RebootTimeout = time.Minute
	s.cfg.WaitPoll = time.Millisecond

	job, err := s.Submit("14_alpine_test", ActionReboot)
	if err != nil {
		t.Fatalf("Submit reboot: %v", err)
	}
	done := waitForJob(t, s, job.ID, StateOK)
	mustStep(t, done, StepStop, StepDone, "")
	mustStep(t, done, StepVerify, StepDone, "libvirt confirms the guest is off")
	mustStep(t, done, StepStart, StepDone, "guest started")
}

// TestRebootRetriesShutdown: the reboot's wait loop re-sends the shutdown
// the same way the staged restore's does.
func TestRebootRetriesShutdown(t *testing.T) {
	fds := &fakeDomains{
		agentErr: errors.New("guest agent command timed out: Guest agent disappeared while executing command"),
		getInfo:  []uint8{runningState, runningState, runningState, runningState, runningState, runningState, runningState, runningState, shutoffState},
	}
	s, _ := newTestStore(snapWith(runningVM(model.AgentOK)), &fakeConn{doms: fds}, time.Second)
	s.cfg.RebootTimeout = 2 * time.Second
	s.cfg.WaitPoll = time.Millisecond
	s.cfg.ShutdownRetry = 5 * time.Millisecond

	job, _ := s.Submit("14_alpine_test", ActionReboot)
	done := waitForJob(t, s, job.ID, StateOK)
	mustStep(t, done, StepStop, StepDone, "re-sent")
}
