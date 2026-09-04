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
// finishes — is the only thing that ever says what the guest is actually
// doing now.
package actions

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"pademelon/internal/clocks"
	"pademelon/internal/libvirtsrc"
	"pademelon/internal/model"
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
)

// ParseAction validates the URL's action segment.
func ParseAction(s string) (Action, error) {
	switch Action(s) {
	case ActionStart, ActionShutdown, ActionReboot, ActionForceOff, ActionPause, ActionResume:
		return Action(s), nil
	default:
		return "", fmt.Errorf("unknown action %q", s)
	}
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
}

// Sentinel errors. The web layer maps them onto status codes; everything
// else becomes a 500 with the detail logged.
var (
	ErrUnknownDomain = errors.New("unknown domain")
	ErrInvalidState  = errors.New("invalid state for action")
	ErrInFlight      = errors.New("action already in flight")
	ErrUnknownAction = errors.New("unknown action")
)

// Config is what the store needs from main.
type Config struct {
	Log          *slog.Logger
	Snapshot     func() model.Snapshot // what the poller last saw
	Conn         libvirtsrc.ConnSource // borrowed libvirt connection
	Nudge        chan<- struct{}       // poked when a job finishes
	AgentTimeout time.Duration         // matches -agent-timeout
	Timeout      time.Duration         // per-job wall clock bound; defaults to clocks.ActionTimeout
	Now          func() time.Time      // injectable clock for tests
	NewID        func() string         // injectable id source for tests
}

// Store is the job registry: an in-memory map with a mutex, single-flight
// per (domain, action), and a lazy sweep for old jobs. No database — the
// registry exists so humans can audit a session, not to survive restarts.
type Store struct {
	cfg  Config
	mu   sync.Mutex
	jobs map[string]*Job
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
	return &Store{cfg: cfg, jobs: map[string]*Job{}}
}

// newID is eight random bytes, hex — unique enough for a session audit and
// short enough to read aloud over a phone call.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the box is in a bad way; a
		// timestamp-based id still beats refusing to act.
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
		// Destroy works on a paused guest too — sometimes that's exactly
		// how you un-wedge one.
		return map[string]bool{"running": true, "paused": true}
	default:
		return nil
	}
}

// Submit validates the request against the last snapshot, registers a job
// and runs it. The bool return says whether the nudge should be expected
// (callers don't care; the runner handles it). Errors are sentinel errors
// for the web layer to map onto status codes.
func (s *Store) Submit(domain string, action Action) (*Job, error) {
	if _, err := ParseAction(string(action)); err != nil {
		return nil, ErrUnknownAction
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

	s.mu.Lock()
	s.sweepLocked(s.cfg.Now())
	for _, j := range s.jobs {
		// Single-flight: a second shutdown while one is pending is a
		// "no, you already asked", not a queued duplicate.
		if j.Domain == domain && j.Action == action &&
			(j.State == StatePending || j.State == StateRunning) {
			inflight := *j
			s.mu.Unlock()
			return &inflight, ErrInFlight
		}
	}
	job := &Job{
		ID:        s.cfg.NewID(),
		Domain:    domain,
		Action:    action,
		State:     StatePending,
		Requested: s.cfg.Now(),
	}
	s.jobs[job.ID] = job
	s.mu.Unlock()

	go s.run(job)
	return clone(job), nil
}

// List returns finished-and-running jobs, oldest first.
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.cfg.Now())
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && out[k].Requested.Before(out[k-1].Requested); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
}

// ShutdownAll plans a graceful shutdown for every running VM with a
// connected agent, staggered so the storage doesn't take a simultaneous
// thundering herd. The plan returns immediately (the HTTP handler must not
// block on a VM-sized loop); the staggered submissions happen in a
// goroutine. Deliberately dumb — the person at the keyboard is the
// orchestrator (IDEAS-EXPLORED.md §3.3).
func (s *Store) ShutdownAll() (planned []string, skipped []string) {
	snap := s.cfg.Snapshot()
	for _, vm := range snap.VMs {
		name := vm.Domain
		switch {
		case !vm.Running:
			// Stopped needs nothing; paused can't hear a graceful shutdown.
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
	go func() {
		done <- s.cfg.Conn.WithConnection(func(doms libvirtsrc.Domains) error {
			return s.execute(doms, job)
		})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(s.cfg.Timeout):
		s.mu.Lock()
		job.State = StateTimeout
		job.Detail = fmt.Sprintf("exceeded the %s action bound; the underlying call was abandoned and the next poll tells the truth", s.cfg.Timeout)
		s.mu.Unlock()
		s.finish(job)
		return
	}

	s.mu.Lock()
	if err != nil {
		job.State = StateFailed
		job.Detail = err.Error()
	} else {
		job.State = StateOK
		job.Detail = s.detailFor(job)
	}
	s.mu.Unlock()
	s.finish(job)
}

// finish pokes the poller so the UI updates in a couple of seconds.
func (s *Store) finish(job *Job) {
	if s.cfg.Nudge != nil {
		select {
		case s.cfg.Nudge <- struct{}{}:
		default:
			// A nudge is already queued; the poll that covers it covers us.
		}
	}
	s.cfg.Log.Info("action finished", "domain", job.Domain, "action", job.Action,
		"state", job.State, "detail", job.Detail)
}

// execute dispatches the verb for a job. It runs inside WithConnection, so
// doms is a live connection.
func (s *Store) execute(doms libvirtsrc.Domains, job *Job) error {
	dom, err := findDomain(doms, job.Domain)
	if err != nil {
		return err
	}
	switch job.Action {
	case ActionStart:
		return doms.DomainCreate(dom)
	case ActionPause:
		return doms.DomainSuspend(dom)
	case ActionResume:
		return doms.DomainResume(dom)
	case ActionReboot:
		// The guest agent has no reboot command, so this is ACPI to the
		// guest OS — same event the power button sends.
		return doms.DomainReboot(dom, libvirt.DomainRebootAcpiPowerBtn)
	case ActionForceOff:
		// The power cord. The confirm dialog already made the human say it
		// twice; the job just does it.
		return doms.DomainDestroy(dom)
	case ActionShutdown:
		return s.shutdown(doms, dom, job.Domain)
	default:
		return fmt.Errorf("unknown action %q", job.Action)
	}
}

// shutdown is graceful, with the fallback ladder from the live tests:
// agent path first (cleanest), ACPI second. guest-shutdown never replies
// on success — the agent exits as its first act — so both an empty reply
// and the "agent disappeared" error mean "requested, very likely working".
// Only something else counts as an agent-path failure worth falling back
// from.
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
// doesn't distinguish it structurally.
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
		return "reboot requested"
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

// clone copies a job so callers can't mutate the registry's copy.
func clone(j *Job) *Job {
	c := *j
	return &c
}
