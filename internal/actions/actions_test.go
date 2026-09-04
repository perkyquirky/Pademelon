package actions

import (
	"errors"
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
// a hypervisor in sight.
type fakeDomains struct {
	calls       []string
	agentReply  string
	agentErr    error
	verbErr     error
	blockCreate chan struct{} // when non-nil, DomainCreate waits on it
}

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
func (f *fakeDomains) DomainReboot(d libvirt.Domain, flags libvirt.DomainRebootFlagValues) error {
	f.calls = append(f.calls, "reboot")
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
	// Release the first job so the goroutine doesn't leak past the test.
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
