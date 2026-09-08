// Package actions is the write side of Pademelon: every libvirt call that
// changes a VM lives here and nowhere else, behind -allow-actions and a
// token. The design is IDEAS-EXPLORED.md §6 in practice — actions are jobs,
// not request work.
//
// Why jobs: actions are asynchronous by nature (a shutdown "succeeds" when
// the guest eventually goes away) and occasionally slow (libvirt's own
// agent-mode shutdown blocked ~58s against a real Windows guest). A request
// that submits a job returns in milliseconds; the job runs on its own
// goroutine with a hard timeout, and the poller — nudged when the job
// finishes — is the only source of truth about what the guest does now.
package actions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"pademelon/internal/clocks"
	"pademelon/internal/libvirtsrc"
	"pademelon/internal/model"
	"pademelon/internal/truenas"
)

// Action is one of the verbs the UI can ask for. The string form is what
// appears in URLs and the job list.
type Action string

const (
	ActionStart    Action = "start"
	ActionShutdown Action = "shutdown"
	ActionReboot   Action = "reboot"
	ActionForceOff Action = "force-off"
	ActionPause    Action = "pause"
	ActionResume   Action = "resume"
	ActionSnapshot Action = "snapshot"

	// Restore and delete target one snapshot each, so they never go
	// through the per-VM verb route: they are not in allActions, the
	// generic route cannot reach them, and their dedicated routes carry
	// the snapshot id. The registry still records them as jobs, so the
	// audit view tells the whole story.
	ActionRestore Action = "restore"
	ActionDelete  Action = "delete"
)

// The two restore paths (§8.6).
const (
	ModeStaged = "staged"
	ModeDirect = "direct"
)

// allActions is the reviewed verb list, in the order the UI shows them.
// ParseAction validates against it and Actions() hands it out — one list,
// so a verb can only be added or removed in one Go place, and the page
// sync test fails until the menu matches.
var allActions = []Action{
	ActionStart,
	ActionShutdown,
	ActionReboot,
	ActionForceOff,
	ActionPause,
	ActionResume,
	ActionSnapshot,
}

// Actions returns the full verb set. Treat the result as read-only.
func Actions() []Action {
	out := make([]Action, len(allActions))
	copy(out, allActions)
	return out
}

// ParseAction validates the URL's action segment.
func ParseAction(s string) (Action, error) {
	for _, a := range allActions {
		if a == Action(s) {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown action %q", s)
}

// Job states, in the order a healthy job moves through them.
const (
	StatePending = "pending"
	StateRunning = "running"
	StateOK      = "ok"
	StateFailed  = "failed"
	StateTimeout = "timeout"
)

// Job is one submitted action. Finished jobs linger for clocks.JobRetention
// so a session's worth of "what did I just do?" has an answer.
type Job struct {
	ID        string    `json:"id"`
	Domain    string    `json:"domain"`
	Action    Action    `json:"action"`
	State     string    `json:"state"`
	Requested time.Time `json:"requested"`
	Detail    string    `json:"detail,omitempty"`

	// Steps is the per-phase progress of a multi-phase job (staged
	// restore, snapshot, reboot), pre-built at submit in execution
	// order and updated in place as the job runs — the UI renders the
	// list as pills that go solid phase by phase. Single-phase verbs
	// (start, pause, delete, ...) carry no steps; their state and
	// detail line tell the whole story.
	Steps []JobStep `json:"steps,omitempty"`

	// Snapshot, Mode, StartAfter and Ack ride restore jobs; Snapshot also
	// rides delete jobs. They are set by the dedicated submit methods and
	// are what the UI's job peek shows. Ack is the operator's signature —
	// the dialog computed what it covers and showed it before clicking.
	Snapshot   string `json:"snapshot,omitempty"`
	Mode       string `json:"mode,omitempty"` // restore: "staged" or "direct"
	StartAfter bool   `json:"startAfter,omitempty"`
	Ack        bool   `json:"ack,omitempty"`
}

// JobStep is one phase of a multi-phase job.
type JobStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Step states, in the order a healthy step moves through them. Skipped
// marks a phase that did not apply (a guest already stopped; a guest
// already quiesced); failed marks the phase that broke the job.
const (
	StepPending = "pending"
	StepActive  = "active"
	StepDone    = "done"
	StepSkipped = "skipped"
	StepFailed  = "failed"
)

// Step names. Stable strings: the page maps them to labels, so renaming
// one is a UI-visible change on purpose.
const (
	StepStop      = "stop"
	StepVerify    = "verify"
	StepRollback  = "rollback"
	StepStart     = "start"
	StepQuiesce   = "quiesce"
	StepSnapshot  = "snapshot"
	StepUnquiesce = "unquiesce"
)

// Sentinel errors. The web layer maps them onto status codes; everything
// else becomes a 500 with the detail logged.
var (
	ErrUnknownDomain = errors.New("unknown domain")
	ErrInvalidState  = errors.New("invalid state for action")
	ErrInFlight      = errors.New("action already in flight")
	ErrUnknownAction = errors.New("unknown action")

	// ErrGuestNotStopped marks a reboot whose guest never powered off
	// within the bound — the job's state reads timeout rather than failed,
	// because nothing broke; the guest simply took too long.
	ErrGuestNotStopped = errors.New("guest did not stop in time")

	// ErrMiddlewareOff marks a snapshot request when the TrueNAS
	// middleware integration is not configured. The dashboard shows
	// snapshot buttons only when the capability says so, so this guards
	// against races between the page and a config change.
	ErrMiddlewareOff = errors.New("middleware integration is off")

	// ErrGuardUnavailable marks a restore or delete whose id could not
	// be checked because the middleware was unreachable at submit time.
	// The web layer maps it to 503 — "try again", not "no such
	// snapshot"; the check fails closed either way.
	ErrGuardUnavailable = errors.New("snapshot list could not be verified")

	// ErrUnknownSnapshot marks a restore or delete whose snapshot id
	// is not in the middleware's live list for that VM — the same
	// blast-radius rule as the action routes: only what the dashboard
	// shows is reachable.
	ErrUnknownSnapshot = errors.New("unknown snapshot for this VM")

	// ErrBadRestore marks a restore request whose options do not parse —
	// an unknown mode, mostly. The web layer maps it to a 400.
	ErrBadRestore = errors.New("bad restore request")
)

// RestoreOpts is what a restore request carries. Mode is "staged" (shut
// down, wait for stopped, roll back, optionally start again) or "direct"
// (roll back in place — the power tool). Ack is the operator's
// acknowledgement, computed by the dialog from what it showed: a direct
// restore of a running VM, and/or newer snapshots that the rollback will
// destroy. Without ack the backend refuses both cases; nothing is
// destroyed by default.
type RestoreOpts struct {
	SnapshotID string
	Mode       string
	StartAfter bool
	Ack        bool
}

// MiddlewareClient is the slice of the TrueNAS middleware the action
// layer is allowed to use — the middleware write verbs live behind this
// interface and are called from here and nowhere else, the same zone rule
// the libvirt verbs follow. nil means the integration is off and
// snapshot/restore/delete jobs are refused before anything is touched.
type MiddlewareClient interface {
	CreateSnapshot(ctx context.Context, dataset, name string) (string, error)
	RollbackSnapshot(ctx context.Context, dataset, name string, recursive bool) error
	DeleteSnapshot(ctx context.Context, dataset, name string) (bool, error)
}

// SnapshotGuard is the id check for restore and delete submissions: is
// this snapshot one the middleware currently knows for this VM? The
// snapshot service implements it with a live middleware answer, so the
// blast-radius rule holds against reality instead of a possibly stale
// poller cache.
type SnapshotGuard interface {
	ValidateKnown(ctx context.Context, domain, snapshotID string) (bool, error)
}

// Config is what the store needs from main.
type Config struct {
	Log           *slog.Logger
	Snapshot      func() model.Snapshot // what the poller last saw
	Conn          libvirtsrc.ConnSource // borrowed libvirt connection
	Nudge         chan<- struct{}       // poked when a job finishes
	AgentTimeout  time.Duration         // matches -agent-timeout
	Timeout       time.Duration         // per-job wall clock bound; defaults to clocks.ActionTimeout
	RebootTimeout time.Duration         // shutdown-then-start wait bound; defaults to clocks.RebootTimeout
	WaitPoll      time.Duration         // reboot's guest-state poll interval; defaults to 2s (injectable for tests)
	ShutdownRetry time.Duration         // how often the wait loop re-sends the shutdown; defaults to clocks.ShutdownRetryInterval

	// SnapshotTimeout bounds one snapshot job (freeze + creates + thaw);
	// defaults to clocks.SnapshotActionTimeout.
	SnapshotTimeout time.Duration

	Truenas MiddlewareClient // TrueNAS middleware writes; nil keeps snapshot jobs refused
	Guard   SnapshotGuard    // snapshot id check for restore/delete; required with Truenas
	Now     func() time.Time // injectable clock for tests
	NewID   func() string    // injectable id source for tests
}

// Store is the job registry: an in-memory map with a mutex, single-flight
// per (domain, action), and a lazy sweep for old jobs. No database — the
// registry exists so humans can audit a session, not to survive restarts.
type Store struct {
	cfg  Config
	mu   sync.Mutex
	jobs map[string]*Job

	// frozen remembers which guests Pademelon froze during a snapshot
	// job and when — the sweep force-thaws any entry that outlives
	// FreezeHoldBound, so a stuck thaw can never leave a hung guest.
	frozen map[string]time.Time
}

// New returns a Store ready to take submissions.
func New(cfg Config) *Store {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = newID
	}
	if cfg.AgentTimeout <= 0 {
		cfg.AgentTimeout = clocks.DefaultAgentTimeout
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = clocks.ActionTimeout
	}
	if cfg.RebootTimeout <= 0 {
		cfg.RebootTimeout = clocks.RebootTimeout
	}
	if cfg.SnapshotTimeout <= 0 {
		cfg.SnapshotTimeout = clocks.SnapshotActionTimeout
	}
	if cfg.WaitPoll <= 0 {
		cfg.WaitPoll = 2 * time.Second
	}
	if cfg.ShutdownRetry <= 0 {
		cfg.ShutdownRetry = clocks.ShutdownRetryInterval
	}
	// A nil *truenas.Client inside the interface is the typed-nil trap
	// (the web layer hit it in production once); treat it as off.
	if v := reflect.ValueOf(cfg.Truenas); v.Kind() == reflect.Ptr && v.IsNil() {
		cfg.Truenas = nil
	}
	if v := reflect.ValueOf(cfg.Guard); v.Kind() == reflect.Ptr && v.IsNil() {
		cfg.Guard = nil
	}
	return &Store{cfg: cfg, jobs: map[string]*Job{}, frozen: map[string]time.Time{}}
}

// newID is eight random bytes, hex — unique enough for a session audit and
// short enough to read aloud over a phone call.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the host is in a bad state; a
		// timestamp-based id is better than refusing to act.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// allowedStates is the action ↔ state table. Checked against the poller's
// last snapshot before anything reaches libvirt — a guest's real state may
// have moved on since, but libvirt will say so and the job will fail with
// the truth in its detail.
func allowedStates(a Action) map[string]bool {
	switch a {
	case ActionStart:
		return map[string]bool{"stopped": true}
	case ActionPause:
		return map[string]bool{"running": true}
	case ActionResume:
		return map[string]bool{"paused": true}
	case ActionShutdown, ActionReboot:
		return map[string]bool{"running": true}
	case ActionForceOff:
		// Destroy works on a paused guest too — sometimes that is the
		// only way to stop a stuck guest.
		return map[string]bool{"running": true, "paused": true}
	case ActionSnapshot:
		// Running guests get frozen (or suspended); a paused or stopped
		// guest is already quiesced by definition.
		return map[string]bool{"running": true, "paused": true, "stopped": true}
	default:
		return nil
	}
}

// register runs the single-flight check and queues a job. A second
// submit of the same (domain, action) while one is pending is a "no, you
// already asked", not a queued duplicate.
func (s *Store) register(domain string, action Action, build func() *Job) (*Job, error) {
	s.mu.Lock()
	s.sweepLocked(s.cfg.Now())
	for _, j := range s.jobs {
		if j.Domain == domain && j.Action == action &&
			(j.State == StatePending || j.State == StateRunning) {
			// clone, not a struct copy: the running job's steps are
			// updated in place, and the returned copy must not alias
			// them mid-encode.
			inflight := clone(j)
			s.mu.Unlock()
			return inflight, ErrInFlight
		}
	}
	job := build()
	stepsFor(job)
	s.jobs[job.ID] = job
	// Clone under the lock: the runner goroutine below writes job fields
	// under this same mutex, and a clone made after releasing it would
	// read them mid-write (the race the -race build caught on 2026-09-08).
	out := clone(job)
	s.mu.Unlock()

	go s.run(job)
	return out, nil
}

// Submit validates the request against the last snapshot, registers a job
// and runs it. The bool return says whether the nudge should be expected
// (callers don't care; the runner handles it). Errors are sentinel errors
// for the web layer to map onto status codes.
func (s *Store) Submit(domain string, action Action) (*Job, error) {
	if _, err := ParseAction(string(action)); err != nil {
		return nil, ErrUnknownAction
	}
	// Middleware-backed verbs are refused at submit time: a config-off
	// integration takes precedence over any state mismatch.
	if action == ActionSnapshot && s.cfg.Truenas == nil {
		return nil, ErrMiddlewareOff
	}

	snap := s.cfg.Snapshot()
	var vm *model.VM
	for i := range snap.VMs {
		if snap.VMs[i].Domain == domain {
			vm = &snap.VMs[i]
			break
		}
	}
	if vm == nil {
		return nil, ErrUnknownDomain
	}
	if !allowedStates(action)[vm.State] {
		return nil, fmt.Errorf("%w: %s is %s, %s needs %s", ErrInvalidState,
			domain, vm.State, action, stateNames(allowedStates(action)))
	}

	return s.register(domain, action, func() *Job {
		return &Job{
			ID:        s.cfg.NewID(),
			Domain:    domain,
			Action:    action,
			State:     StatePending,
			Requested: s.cfg.Now(),
		}
	})
}

// SubmitRestore queues a snapshot restore. Validation happens here, on a
// live middleware answer (see validateSnapshot): the snapshot must belong
// to one of this VM's disk datasets, a direct restore of a running VM
// needs the acknowledgement, and the mode must be one of the two known
// paths. The job itself re-checks nothing — the registry owns it from
// here. The context bounds the validation wait, not the job.
func (s *Store) SubmitRestore(ctx context.Context, domain string, opts RestoreOpts) (*Job, error) {
	if s.cfg.Truenas == nil {
		return nil, ErrMiddlewareOff
	}
	if opts.Mode != ModeStaged && opts.Mode != ModeDirect {
		return nil, fmt.Errorf("%w: mode %q, want %q or %q", ErrBadRestore, opts.Mode, ModeStaged, ModeDirect)
	}
	if err := s.validateSnapshot(ctx, domain, opts.SnapshotID); err != nil {
		return nil, err
	}
	if opts.Mode == ModeDirect {
		if vm := s.lookupVM(s.cfg.Snapshot(), domain); vm != nil && vm.Running && !opts.Ack {
			return nil, fmt.Errorf(
				"%w: %s is running — a direct rollback under a running guest can corrupt the disk; use the staged restore, stop the VM first, or pass the acknowledgement",
				ErrInvalidState, domain)
		}
	}
	return s.register(domain, ActionRestore, func() *Job {
		return &Job{
			ID:         s.cfg.NewID(),
			Domain:     domain,
			Action:     ActionRestore,
			Snapshot:   opts.SnapshotID,
			Mode:       opts.Mode,
			StartAfter: opts.StartAfter,
			Ack:        opts.Ack,
			State:      StatePending,
			Requested:  s.cfg.Now(),
		}
	})
}

// SubmitDelete queues a snapshot deletion. The id guard is the same as
// the restore's: only snapshots the middleware currently knows are
// reachable. The context bounds the validation wait, not the job.
func (s *Store) SubmitDelete(ctx context.Context, domain, snapshotID string) (*Job, error) {
	if s.cfg.Truenas == nil {
		return nil, ErrMiddlewareOff
	}
	if err := s.validateSnapshot(ctx, domain, snapshotID); err != nil {
		return nil, err
	}
	return s.register(domain, ActionDelete, func() *Job {
		return &Job{
			ID:        s.cfg.NewID(),
			Domain:    domain,
			Action:    ActionDelete,
			Snapshot:  snapshotID,
			State:     StatePending,
			Requested: s.cfg.Now(),
		}
	})
}

// validateSnapshot checks the id guard: the snapshot must be one the
// middleware currently knows for this VM. The check runs at submit time
// against the snapshot service — a clean held list answers instantly, a
// missing or stale one costs a fresh middleware fetch — so a stale or
// fabricated id is refused before a job exists. The wait is bounded by
// ctx; the web layer hands in a bounded one.
func (s *Store) validateSnapshot(ctx context.Context, domain, id string) error {
	if s.cfg.Guard == nil {
		return fmt.Errorf("%w: no snapshot guard is configured", ErrGuardUnavailable)
	}
	ok, err := s.cfg.Guard.ValidateKnown(ctx, domain, id)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrGuardUnavailable, id, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s on %s", ErrUnknownSnapshot, id, domain)
	}
	if _, _, cut := strings.Cut(id, "@"); !cut {
		return fmt.Errorf("%w: malformed id %q", ErrUnknownSnapshot, id)
	}
	return nil
}

// List returns running and finished jobs, oldest first.
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.cfg.Now())
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		// clone, not a struct copy: the runner updates steps in place
		// under the lock, and a shallow copy would alias that backing
		// array into the caller's JSON encoding mid-write.
		out = append(out, *clone(j))
	}
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && out[k].Requested.Before(out[k-1].Requested); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
}

// ShutdownAll plans a graceful shutdown for every running VM with a
// connected agent, staggered so the storage does not see a simultaneous
// load from every VM. The plan returns immediately (the HTTP handler must
// not block on a VM-sized loop); the staggered submissions happen in a
// goroutine. The plan is deliberately simple — the person at the keyboard
// is the orchestrator (IDEAS-EXPLORED.md §3.3).
func (s *Store) ShutdownAll() (planned []string, skipped []string) {
	snap := s.cfg.Snapshot()
	for _, vm := range snap.VMs {
		name := vm.Domain
		switch {
		case !vm.Running:
			// Stopped needs nothing; a paused guest cannot hear a
			// graceful shutdown.
			if vm.State == "paused" {
				skipped = append(skipped, name+" (paused — left alone)")
			}
			continue
		case vm.Agent != model.AgentOK:
			skipped = append(skipped, name+" (no agent — shut it down individually for the ACPI path)")
			continue
		}
		planned = append(planned, name)
	}
	if len(planned) == 0 {
		return planned, skipped
	}

	go func() {
		for i, name := range planned {
			if i > 0 {
				time.Sleep(time.Second)
			}
			if _, err := s.Submit(name, ActionShutdown); err != nil {
				s.cfg.Log.Warn("bulk shutdown submission failed", "domain", name, "err", err)
			}
		}
	}()
	return planned, skipped
}

// run executes the job off the request path. Every exit updates the job,
// and every exit nudges the poller so the UI reflects reality within a
// poll nudge's time instead of a full interval.
func (s *Store) run(job *Job) {
	s.mu.Lock()
	job.State = StateRunning
	s.mu.Unlock()

	done := make(chan error, 1)
	detail := ""
	go func() {
		done <- s.cfg.Conn.WithConnection(func(doms libvirtsrc.Domains) error {
			// A dynamic detail (the snapshot job's closing line) is
			// written here and read after the channel send — the
			// channel's happens-before makes that safe without a lock.
			var err error
			detail, err = s.execute(doms, job)
			return err
		})
	}()

	var err error
	// A reboot includes the wait-for-shutdown phase. Its outer bound gets
	// a minute of slack over the reboot's own deadline, so the specific
	// "guest did not power off" message fires before this catch-all
	// timer. A staged restore includes that same wait — its bound must
	// cover it too, or the job would be marked timeout mid-sequence
	// while the steps kept advancing. A snapshot job gets its own, longer
	// bound: freeze, one create per dataset and the thaw are one
	// sequence, and even a timed-out job keeps running it — the thaw
	// still lands. A direct restore is a quick rollback and keeps the
	// default.
	bound := s.cfg.Timeout
	switch job.Action {
	case ActionReboot:
		bound = s.cfg.RebootTimeout + time.Minute
	case ActionRestore:
		if job.Mode == ModeStaged {
			bound = s.cfg.RebootTimeout + time.Minute
		}
	case ActionSnapshot:
		bound = s.cfg.SnapshotTimeout
	}
	select {
	case err = <-done:
	case <-time.After(bound):
		s.mu.Lock()
		job.State = StateTimeout
		job.Detail = fmt.Sprintf("exceeded the %s action bound; the underlying call was abandoned and the next poll tells the truth", bound)
		// Whichever phase the job was in when the bound fired is the
		// phase that did not land — the pills must agree with the
		// verdict.
		for i := range job.Steps {
			if job.Steps[i].State == StepActive {
				job.Steps[i].State = StepFailed
			}
		}
		s.mu.Unlock()
		s.finish(job)
		return
	}

	s.mu.Lock()
	if err != nil {
		job.State = StateFailed
		if errors.Is(err, ErrGuestNotStopped) {
			job.State = StateTimeout
		}
		job.Detail = err.Error()
	} else {
		job.State = StateOK
		job.Detail = detail
		if job.Detail == "" {
			job.Detail = s.detailFor(job)
		}
	}
	s.mu.Unlock()
	s.finish(job)
}

// finish pokes the poller so the UI updates in a couple of seconds.
func (s *Store) finish(job *Job) {
	s.nudgePoller()
	s.cfg.Log.Info("action finished", "domain", job.Domain, "action", job.Action,
		"state", job.State, "detail", job.Detail)
}

// nudgePoller pokes the poll loop for an early poll. Mid-job callers use
// it when a phase changes something the poller will report (the guest
// turning off, starting again), so the row catches up in one nudge
// interval instead of a full poll.
func (s *Store) nudgePoller() {
	if s.cfg.Nudge == nil {
		return
	}
	select {
	case s.cfg.Nudge <- struct{}{}:
	default:
		// A nudge is already queued; the poll that covers it covers us.
	}
}

// setStep moves one of the job's steps to a new state. An empty detail
// leaves the step's existing wording alone, so a phase can go active →
// done without erasing what it did. Called from the runner goroutine;
// the write sits under the same mutex every reader uses.
func (s *Store) setStep(job *Job, name, state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range job.Steps {
		if job.Steps[i].Name == name {
			job.Steps[i].State = state
			if detail != "" {
				job.Steps[i].Detail = detail
			}
			return
		}
	}
	job.Steps = append(job.Steps, JobStep{Name: name, State: state, Detail: detail})
}

// stepsFor pre-builds a job's step list, in execution order. Phases that
// may not apply stay pending — the runner marks them skipped when they
// don't (a guest already stopped, a guest already quiesced), which the
// UI shows as a greyed pill rather than hiding the phase.
func stepsFor(job *Job) {
	switch job.Action {
	case ActionRestore:
		if job.Mode == ModeStaged {
			job.Steps = []JobStep{
				{Name: StepStop, State: StepPending},
				{Name: StepVerify, State: StepPending},
				{Name: StepRollback, State: StepPending},
			}
			if job.StartAfter {
				job.Steps = append(job.Steps, JobStep{Name: StepStart, State: StepPending})
			}
		} else {
			job.Steps = []JobStep{{Name: StepRollback, State: StepPending}}
		}
	case ActionSnapshot:
		job.Steps = []JobStep{
			{Name: StepQuiesce, State: StepPending},
			{Name: StepSnapshot, State: StepPending},
			{Name: StepUnquiesce, State: StepPending},
		}
	case ActionReboot:
		job.Steps = []JobStep{
			{Name: StepStop, State: StepPending},
			{Name: StepVerify, State: StepPending},
			{Name: StepStart, State: StepPending},
		}
	}
}

// execute dispatches the verb for a job. It runs inside WithConnection, so
// doms is a live connection. The string is an optional closing line for the
// job's detail; empty falls back to detailFor's static text.
func (s *Store) execute(doms libvirtsrc.Domains, job *Job) (string, error) {
	// Delete never touches libvirt — a stopped or vanished VM still has
	// snapshots worth deleting, so it must not depend on finding a domain.
	if job.Action == ActionDelete {
		return s.executeDelete(job)
	}
	dom, err := findDomain(doms, job.Domain)
	if err != nil {
		return "", err
	}
	switch job.Action {
	case ActionStart:
		return "", doms.DomainCreate(dom)
	case ActionPause:
		return "", doms.DomainSuspend(dom)
	case ActionResume:
		return "", doms.DomainResume(dom)
	case ActionReboot:
		return "", s.reboot(doms, dom, job)
	case ActionForceOff:
		// Force off. The confirm dialog already made the user say it
		// twice; the job just runs it.
		return "", doms.DomainDestroy(dom)
	case ActionShutdown:
		return "", s.shutdown(doms, dom, job.Domain)
	case ActionSnapshot:
		return s.snapshot(doms, dom, job)
	case ActionRestore:
		return s.restore(doms, dom, job)
	default:
		return "", fmt.Errorf("unknown action %q", job.Action)
	}
}

// restore is the §8.6 sequence. Staged: shut the guest down, wait until
// libvirt agrees it is off, roll back, optionally start again. Direct:
// roll back in place — the submit-time ack is the operator's signature on
// the risks, and the recursive flag is how newer snapshots get destroyed
// on the way. Each phase reports into the job's steps, so the UI can show
// the guest being confirmed off before anything is rewound.
func (s *Store) restore(doms libvirtsrc.Domains, dom libvirt.Domain, job *Job) (string, error) {
	dataset, name, ok := strings.Cut(job.Snapshot, "@")
	if !ok {
		return "", fmt.Errorf("malformed snapshot id %q", job.Snapshot)
	}

	narration := ""
	if job.Mode == ModeStaged {
		state, _, _, _, _, err := doms.DomainGetInfo(dom)
		if err != nil {
			s.setStep(job, StepVerify, StepFailed, "could not read the guest state: "+err.Error())
			return "", fmt.Errorf("restore: read guest state: %w", err)
		}
		if libvirt.DomainState(state) != libvirt.DomainShutoff {
			s.setStep(job, StepStop, StepActive, "shutdown request sent")
			if err := s.shutdown(doms, dom, job.Domain); err != nil {
				s.setStep(job, StepStop, StepFailed, "shutdown request failed: "+err.Error())
				return "", fmt.Errorf("restore: shutdown phase: %w", err)
			}
			s.setStep(job, StepStop, StepDone, "")
			s.setStep(job, StepVerify, StepActive, "waiting for libvirt to confirm the guest is off")
			shutAt := s.cfg.Now()
			resend := func(attempt int) {
				note := fmt.Sprintf("shutdown re-sent (attempt %d) — some guests ignore the first", attempt)
				if err := s.shutdown(doms, dom, job.Domain); err != nil {
					note += "; re-send failed: " + err.Error()
				}
				s.setStep(job, StepStop, StepDone, note)
			}
			if err := s.waitStopped(doms, dom, "force off, or roll back directly once it is stopped", resend); err != nil {
				s.setStep(job, StepVerify, StepFailed,
					fmt.Sprintf("libvirt still reports the guest on after %s — force off, or roll back directly once it is stopped",
						s.cfg.RebootTimeout))
				return "", fmt.Errorf("restore: %w", err)
			}
			s.setStep(job, StepVerify, StepDone,
				"libvirt confirms the guest is off ("+s.cfg.Now().Sub(shutAt).Round(time.Second).String()+")")
			s.nudgePoller()
			narration = "guest shut down, "
		} else {
			s.setStep(job, StepStop, StepSkipped, "guest was already stopped")
			s.setStep(job, StepVerify, StepDone, "libvirt confirms the guest is already off")
		}
	} else if job.Mode != ModeDirect {
		return "", fmt.Errorf("%w: mode %q", ErrBadRestore, job.Mode)
	}

	s.setStep(job, StepRollback, StepActive, "rewinding "+dataset)
	if err := s.cfg.Truenas.RollbackSnapshot(context.Background(), dataset, name, job.Ack); err != nil {
		s.setStep(job, StepRollback, StepFailed, err.Error())
		return "", fmt.Errorf("restore: rollback failed, the guest is left as-is: %w", err)
	}
	s.setStep(job, StepRollback, StepDone, "rolled back to "+job.Snapshot)

	var detail string
	switch {
	case job.Mode == ModeStaged && job.StartAfter:
		s.setStep(job, StepStart, StepActive, "starting the guest again")
		if err := doms.DomainCreate(dom); err != nil {
			s.setStep(job, StepStart, StepFailed, err.Error())
			return "", fmt.Errorf("restore: rolled back but start failed: %w", err)
		}
		s.setStep(job, StepStart, StepDone, "guest started")
		s.nudgePoller()
		detail = fmt.Sprintf("restored %q — %srolled back, started again", job.Snapshot, narration)
	case job.Mode == ModeStaged:
		detail = fmt.Sprintf("restored %q — %srolled back, guest left stopped", job.Snapshot, narration)
	default:
		detail = fmt.Sprintf("restored %q — rolled back in place", job.Snapshot)
	}
	if job.Ack {
		detail += ", newer snapshots destroyed"
	}
	return detail, nil
}

// executeDelete removes one snapshot through the middleware. Already-gone
// is a fine answer, not a failure.
func (s *Store) executeDelete(job *Job) (string, error) {
	dataset, name, ok := strings.Cut(job.Snapshot, "@")
	if !ok {
		return "", fmt.Errorf("malformed snapshot id %q", job.Snapshot)
	}
	deleted, err := s.cfg.Truenas.DeleteSnapshot(context.Background(), dataset, name)
	if err != nil {
		return "", err
	}
	if deleted {
		return fmt.Sprintf("snapshot %q deleted", job.Snapshot), nil
	}
	return fmt.Sprintf("snapshot %q was already gone", job.Snapshot), nil
}

// snapshot is the consistent-snapshot sequence from IDEAS-EXPLORED.md §8.4:
// quiesce the guest, snapshot every disk dataset on the middleware, then
// un-quiesce — with the un-quiesce guaranteed on every path, because a
// frozen guest is a hung guest.
//
// The quiesce ladder: fsfreeze when the agent answers (the cleanest), a
// suspend/resume for agentless or old-agent guests, and nothing at all for
// a guest that is already paused — paused is quiesced by definition.
func (s *Store) snapshot(doms libvirtsrc.Domains, dom libvirt.Domain, job *Job) (string, error) {
	if s.cfg.Truenas == nil {
		return "", ErrMiddlewareOff
	}
	snap := s.cfg.Snapshot()
	vm := s.lookupVM(snap, job.Domain)
	if vm == nil {
		return "", fmt.Errorf("%w: %s not in the last snapshot", ErrUnknownDomain, job.Domain)
	}
	datasets := truenasDatasets(vm)
	if len(datasets) == 0 {
		return "", fmt.Errorf("%s has no zvol-backed disks to snapshot", job.Domain)
	}
	name := snapshotName(vm, s.cfg.Now())

	// --- quiesce phase ------------------------------------------------
	froze, suspended := false, false
	var freezeErr error
	if vm.State == "running" {
		if vm.Agent == model.AgentOK {
			s.setStep(job, StepQuiesce, StepActive, "freezing the guest's filesystems")
			_, freezeErr = rawAgentCall(doms, dom, int32(s.cfg.AgentTimeout/time.Second), `{"execute":"guest-fsfreeze-freeze"}`)
			if freezeErr == nil {
				froze = true
				s.frozenMark(job.Domain)
			} else {
				// An old agent without fsfreeze, or a transient hiccup —
				// the suspend fallback quiesces just as well.
				s.logDebug("fsfreeze failed, falling back to suspend", job.Domain, freezeErr)
				s.setStep(job, StepQuiesce, StepActive, "fsfreeze failed, falling back to suspend")
			}
		} else {
			s.setStep(job, StepQuiesce, StepActive, "suspending the guest (no working agent for fsfreeze)")
		}
		if !froze {
			if err := doms.DomainSuspend(dom); err != nil {
				s.setStep(job, StepQuiesce, StepFailed,
					fmt.Sprintf("could not quiesce the guest (freeze: %v, suspend: %v)", freezeErr, err))
				return "", fmt.Errorf("could not quiesce the guest (freeze: %v, suspend: %w)", freezeErr, err)
			}
			suspended = true
		}
		switch {
		case froze:
			s.setStep(job, StepQuiesce, StepDone, "guest filesystems frozen")
		case suspended:
			s.setStep(job, StepQuiesce, StepDone, "guest paused for the shot")
		}
	} else {
		s.setStep(job, StepQuiesce, StepSkipped, "guest already quiesced ("+vm.State+")")
	}

	// --- snapshot phase -------------------------------------------------
	// One create per dataset; a partial failure keeps going so the
	// datasets that made it are reported rather than secretly dropped —
	// no surprise deletions to "clean up".
	s.setStep(job, StepSnapshot, StepActive,
		fmt.Sprintf("creating %q on %d dataset(s)", name, len(datasets)))
	var created, failed []string
	for _, ds := range datasets {
		_, err := s.cfg.Truenas.CreateSnapshot(context.Background(), ds, name)
		if err != nil {
			failed = append(failed, ds+": "+err.Error())
		} else {
			created = append(created, ds)
		}
	}
	if len(failed) > 0 {
		s.setStep(job, StepSnapshot, StepFailed,
			fmt.Sprintf("failed on %d of %d dataset(s): %s", len(failed), len(datasets), strings.Join(failed, "; ")))
	} else {
		s.setStep(job, StepSnapshot, StepDone,
			fmt.Sprintf("created %q on %d dataset(s)", name, len(created)))
	}

	// --- un-quiesce phase — runs on every path below --------------------
	var unquiesceErrs []string
	if froze {
		s.setStep(job, StepUnquiesce, StepActive, "thawing the guest's filesystems")
		if err := s.thaw(doms, dom, job.Domain); err != nil {
			unquiesceErrs = append(unquiesceErrs,
				"WARNING: guest may still be frozen — retry the snapshot's thaw via the sweep, or thaw manually: "+err.Error())
			s.setStep(job, StepUnquiesce, StepFailed,
				"WARNING: guest may still be frozen — retry the snapshot's thaw via the sweep, or thaw manually: "+err.Error())
		} else {
			s.setStep(job, StepUnquiesce, StepDone, "guest filesystems thawed")
		}
	}
	if suspended {
		s.setStep(job, StepUnquiesce, StepActive, "resuming the guest")
		if err := doms.DomainResume(dom); err != nil {
			unquiesceErrs = append(unquiesceErrs, "WARNING: guest may still be paused: "+err.Error())
			s.setStep(job, StepUnquiesce, StepFailed, "WARNING: guest may still be paused: "+err.Error())
		} else {
			s.setStep(job, StepUnquiesce, StepDone, "guest resumed")
		}
	}
	if !froze && !suspended {
		s.setStep(job, StepUnquiesce, StepSkipped, "nothing to un-quiesce")
	}

	// --- report -----------------------------------------------------------
	quiesceNote := ""
	if len(unquiesceErrs) > 0 {
		quiesceNote = " " + strings.Join(unquiesceErrs, " ")
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("snapshot %q failed on %d of %d dataset(s): %s.%s",
			name, len(failed), len(datasets), strings.Join(failed, "; "), quiesceNote)
	}
	detail := fmt.Sprintf("snapshot %q created on %d dataset(s)", name, len(created))
	switch {
	case froze:
		detail += " — guest filesystems frozen during the shot, thawed after"
	case suspended:
		detail += " — guest paused during the shot, resumed after"
	default:
		detail += " — guest was already quiesced (paused or stopped)"
	}
	return detail + quiesceNote, nil
}

// thaw unfreezes the guest's filesystems, retrying once — the guest's
// agent may be busy with its own shutdown the first time. It clears the
// sweep marker only on success, so a persistent failure keeps the
// sweep's attention.
func (s *Store) thaw(doms libvirtsrc.Domains, dom libvirt.Domain, domain string) error {
	_, err := rawAgentCall(doms, dom, int32(s.cfg.AgentTimeout/time.Second), `{"execute":"guest-fsfreeze-thaw"}`)
	if err == nil {
		s.frozenClear(domain)
		return nil
	}
	time.Sleep(2 * time.Second)
	_, err = rawAgentCall(doms, dom, int32(s.cfg.AgentTimeout/time.Second), `{"execute":"guest-fsfreeze-thaw"}`)
	if err == nil {
		s.frozenClear(domain)
		return nil
	}
	return err
}

// lookupVM finds one VM in the last snapshot.
func (s *Store) lookupVM(snap model.Snapshot, domain string) *model.VM {
	for i := range snap.VMs {
		if snap.VMs[i].Domain == domain {
			return &snap.VMs[i]
		}
	}
	return nil
}

// truenasDatasets maps a VM's disks to middleware dataset names. Anything
// that is not a zvol (ISOs, file-backed images) is skipped — there is no
// zvol to snapshot.
func truenasDatasets(vm *model.VM) []string {
	var out []string
	for _, d := range vm.Disks {
		if ds, ok := truenas.DatasetFromDiskSource(d.Source); ok {
			out = append(out, ds)
		}
	}
	return out
}

// snapshotName builds the middleware snapshot name: pademelon-<vm>-<ts>.
// The prefix is what makes these snapshots recognisable — and filterable —
// in both the TrueNAS UI and Pademelon's own list. Characters that ZFS
// rejects are folded to dashes.
func snapshotName(vm *model.VM, at time.Time) string {
	base := vm.Name
	if base == "" {
		base = vm.Domain
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-' || r == '.':
			return r
		default:
			return '-'
		}
	}, base)
	return "pademelon-" + base + "-" + at.Format("2006-01-02_15-04-05")
}

// frozenMark / frozenClear / frozenExpired keep the sweep's bookkeeping.
func (s *Store) frozenMark(domain string) {
	s.mu.Lock()
	s.frozen[domain] = time.Now()
	s.mu.Unlock()
}

func (s *Store) frozenClear(domain string) {
	s.mu.Lock()
	delete(s.frozen, domain)
	s.mu.Unlock()
}

// SweepFrozenOnce is the startup sweep: every running guest with a
// connected agent is asked for its fsfreeze status, and anything found
// frozen gets thawed. It covers the crash case — a freeze succeeded, the
// process died before the thaw, and the in-memory marker died with it; the
// guest did not. Called from main in a goroutine; waits briefly for the
// first poll so the agent states are known.
func (s *Store) SweepFrozenOnce(ctx context.Context) {
	for i := 0; i < 12; i++ {
		if len(s.cfg.Snapshot().VMs) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	s.sweepAgents(ctx, nil)
}

// SweepLoop force-thaws any guest whose frozen marker outlives
// FreezeHoldBound — a second guard behind the job's own guaranteed thaw.
// It runs until ctx is done; main starts it alongside the other loops.
func (s *Store) SweepLoop(ctx context.Context) {
	ticker := time.NewTicker(clocks.FreezeHoldBound)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			var expired []string
			for domain, since := range s.frozen {
				if time.Since(since) > clocks.FreezeHoldBound {
					expired = append(expired, domain)
				}
			}
			s.mu.Unlock()
			for _, domain := range expired {
				s.forceThaw(ctx, domain)
			}
		}
	}
}

// forceThaw thaws one tracked guest no matter what — the sweep's direct
// thaw. The marker clears on success; a persistent failure keeps it and
// the next sweep tries again.
func (s *Store) forceThaw(ctx context.Context, domain string) {
	s.cfg.Log.Warn("guest frozen longer than the hold bound; force-thawing", "domain", domain)
	err := s.cfg.Conn.WithConnection(func(doms libvirtsrc.Domains) error {
		dom, err := findDomain(doms, domain)
		if err != nil {
			return err
		}
		return s.thaw(doms, dom, domain)
	})
	if err != nil {
		s.cfg.Log.Warn("force-thaw failed; the sweep will retry", "domain", domain, "err", err)
	}
}

// sweepAgents asks every running guest with a connected agent for its
// fsfreeze status and thaws anything frozen. The optional filter limits
// the pass to specific domains (nil = everyone).
func (s *Store) sweepAgents(ctx context.Context, only map[string]bool) {
	snap := s.cfg.Snapshot()
	err := s.cfg.Conn.WithConnection(func(doms libvirtsrc.Domains) error {
		for _, vm := range snap.VMs {
			if only != nil && !only[vm.Domain] {
				continue
			}
			if vm.State != "running" || vm.Agent != model.AgentOK {
				continue
			}
			dom, err := findDomain(doms, vm.Domain)
			if err != nil {
				continue // not running right now; nothing to thaw
			}
			raw, err := rawAgentCall(doms, dom, int32(s.cfg.AgentTimeout/time.Second), `{"execute":"guest-fsfreeze-status"}`)
			if err != nil {
				continue // a quiet agent isn't a frozen guest
			}
			var r struct {
				Return string `json:"return"`
			}
			if json.Unmarshal([]byte(raw), &r) != nil || r.Return != "frozen" {
				continue
			}
			s.cfg.Log.Warn("found a guest frozen outside any snapshot job; thawing", "domain", vm.Domain)
			s.frozenMark(vm.Domain)
			if err := s.thaw(doms, dom, vm.Domain); err != nil {
				s.cfg.Log.Warn("startup thaw failed; the sweep will retry", "domain", vm.Domain, "err", err)
			}
		}
		return nil
	})
	if err != nil {
		s.cfg.Log.Warn("fsfreeze sweep could not reach libvirt", "err", err)
	}
}

// waitStopped polls libvirt until the guest is actually off, bounded by
// RebootTimeout. Shared by the reboot and the staged restore — the same
// patience, the same honest timeout, the same "left untouched" outcome.
// Some guests ignore the first shutdown request (an Alpine test guest
// needed a second), so resend fires every ShutdownRetryInterval while
// waiting: attempt 1 is the original request, attempt 2 the first
// re-send. A nil resend just waits.
func (s *Store) waitStopped(doms libvirtsrc.Domains, dom libvirt.Domain, hint string, resend func(attempt int)) error {
	deadline := s.cfg.Now().Add(s.cfg.RebootTimeout)
	lastSent := s.cfg.Now()
	for attempt := 1; ; {
		state, _, _, _, _, err := doms.DomainGetInfo(dom)
		if err != nil {
			return fmt.Errorf("waiting for shutdown: %w", err)
		}
		if libvirt.DomainState(state) == libvirt.DomainShutoff {
			return nil
		}
		if s.cfg.Now().After(deadline) {
			return fmt.Errorf("%w within %s — %s", ErrGuestNotStopped, s.cfg.RebootTimeout, hint)
		}
		if resend != nil && s.cfg.Now().Sub(lastSent) >= s.cfg.ShutdownRetry {
			attempt++
			resend(attempt)
			lastSent = s.cfg.Now()
		}
		time.Sleep(s.cfg.WaitPoll)
	}
}

// reboot is the only reliable "restart" there is: there is no guest-agent
// reboot command, and the ACPI power button means whatever the guest's OS
// decides it means (Windows defaults to shut down; the Alpine test guest
// ignored it entirely). So the code does what TrueNAS middleware itself
// does — shut down gracefully, wait for the guest to actually stop, then
// start it again. If the guest never stops, the job ends as a timeout
// with instructions and the VM is left untouched rather than
// half-rebooted.
func (s *Store) reboot(doms libvirtsrc.Domains, dom libvirt.Domain, job *Job) error {
	s.setStep(job, StepStop, StepActive, "shutdown request sent")
	if err := s.shutdown(doms, dom, job.Domain); err != nil {
		s.setStep(job, StepStop, StepFailed, "shutdown request failed: "+err.Error())
		return fmt.Errorf("shutdown phase: %w", err)
	}
	s.setStep(job, StepStop, StepDone, "")
	s.setStep(job, StepVerify, StepActive, "waiting for libvirt to confirm the guest is off")
	resend := func(attempt int) {
		note := fmt.Sprintf("shutdown re-sent (attempt %d) — some guests ignore the first", attempt)
		if err := s.shutdown(doms, dom, job.Domain); err != nil {
			note += "; re-send failed: " + err.Error()
		}
		s.setStep(job, StepStop, StepDone, note)
	}
	if err := s.waitStopped(doms, dom, "force off or start it manually", resend); err != nil {
		s.setStep(job, StepVerify, StepFailed,
			fmt.Sprintf("libvirt still reports the guest on after %s — force off or start it manually", s.cfg.RebootTimeout))
		return err
	}
	s.setStep(job, StepVerify, StepDone, "libvirt confirms the guest is off")
	s.nudgePoller()
	s.setStep(job, StepStart, StepActive, "starting the guest again")
	if err := doms.DomainCreate(dom); err != nil {
		s.setStep(job, StepStart, StepFailed, err.Error())
		return fmt.Errorf("guest stopped but start failed: %w", err)
	}
	s.setStep(job, StepStart, StepDone, "guest started")
	s.nudgePoller()
	return nil
}

// shutdown is graceful, with the fallback ladder from the live tests:
// agent path first (cleanest), ACPI second. guest-shutdown never replies
// on success — the agent exits as its first act — so both an empty reply
// and the "agent disappeared" error mean "requested, very likely working".
// Only something else counts as an agent-path failure that deserves the
// ACPI fallback.
func (s *Store) shutdown(doms libvirtsrc.Domains, dom libvirt.Domain, domain string) (err error) {
	if s.agentConnected(domain) {
		_, callErr := rawAgentCall(doms, dom, int32(s.cfg.AgentTimeout/time.Second), `{"execute":"guest-shutdown"}`)
		if callErr == nil {
			return nil
		}
		if isAgentGoneErr(callErr) {
			return nil
		}
		s.logDebug("agent shutdown failed, falling back to ACPI", domain, callErr)
	}
	return doms.DomainShutdownFlags(dom, libvirt.DomainShutdownAcpiPowerBtn)
}

// agentConnected reads the last snapshot: did this VM's channel say
// "connected" at the last poll?
func (s *Store) agentConnected(domain string) bool {
	for _, vm := range s.cfg.Snapshot().VMs {
		if vm.Domain == domain {
			return vm.Agent == model.AgentOK
		}
	}
	return false
}

// rawAgentCall sends one agent command and tolerates an empty reply — for
// guest-shutdown, empty is the success case. (The read-side caller in the
// poller rejects empties; here the semantics are different.)
func rawAgentCall(doms libvirtsrc.Domains, d libvirt.Domain, timeoutSecs int32, cmd string) (string, error) {
	res, err := doms.QEMUDomainAgentCommand(d, cmd, timeoutSecs, 0)
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		return "", nil
	}
	return res[0], nil
}

// isAgentGoneErr is the success signature discovered by the live tests:
// "guest agent command timed out: Guest agent disappeared while executing
// command" — near-instant on Linux, ~4s on Windows, and the guest shuts
// down either way. Matched on the message because libvirt's error type
// does not distinguish it structurally.
func isAgentGoneErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Guest agent disappeared")
}

// findDomain resolves a domain name to its handle.
func findDomain(doms libvirtsrc.Domains, name string) (libvirt.Domain, error) {
	doms_, _, err := doms.ConnectListAllDomains(1, 0)
	if err != nil {
		return libvirt.Domain{}, fmt.Errorf("list domains: %w", err)
	}
	for _, d := range doms_ {
		if d.Name == name {
			return d, nil
		}
	}
	return libvirt.Domain{}, fmt.Errorf("%w: %s not running right now", ErrUnknownDomain, name)
}

// detailFor gives a finished job a human-readable closing line.
func (s *Store) detailFor(job *Job) string {
	switch job.Action {
	case ActionShutdown:
		return "shutdown requested — the guest decides how fast"
	case ActionReboot:
		return "reboot complete — the guest shut down and was started again"
	case ActionStart, ActionPause, ActionResume:
		return string(job.Action) + " requested"
	case ActionForceOff:
		return "force off sent"
	default:
		return ""
	}
}

// sweepLocked drops jobs older than JobRetention. Called with the lock held.
func (s *Store) sweepLocked(now time.Time) {
	for id, j := range s.jobs {
		if now.Sub(j.Requested) > clocks.JobRetention {
			delete(s.jobs, id)
		}
	}
}

func (s *Store) logDebug(msg, domain string, err error) {
	s.cfg.Log.Debug(msg, "domain", domain, "err", err)
}

// stateNames renders an allowlist for error messages: "running or paused".
func stateNames(states map[string]bool) string {
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	return strings.Join(names, " or ")
}

// clone copies a job so callers cannot mutate the registry's copy.
// Steps get a real copy: the runner updates them under the lock, and a
// shared backing array would leak those writes into returned jobs.
func clone(j *Job) *Job {
	c := *j
	if len(j.Steps) > 0 {
		c.Steps = make([]JobStep, len(j.Steps))
		copy(c.Steps, j.Steps)
	}
	return &c
}
