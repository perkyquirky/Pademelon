package libvirtsrc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"

	"pademelon/internal/clocks"
)

func stat(tag int32, val uint64) libvirt.DomainMemoryStat {
	return libvirt.DomainMemoryStat{Tag: tag, Val: val}
}

func TestBalloonMemory(t *testing.T) {
	// Whole seconds only, so staleness arithmetic is exact.
	now := time.Unix(1788220800, 0) // 2026-09-01 00:00 UTC

	tests := []struct {
		name      string
		stats     []libvirt.DomainMemoryStat
		wantUsed  uint64
		wantTotal uint64
		wantOK    bool
		wantStale bool
	}{
		{
			// A healthy 1.5 GiB VM: used comes out as total minus
			// MemAvailable, the same number `free` calls used.
			name: "fresh stats",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1438720),
				stat(memStatUsable, 774144),
				stat(memStatUnused, 198656),
				stat(memStatLastUpdate, uint64(now.Add(-10*time.Second).Unix())),
			},
			wantUsed:  664576,
			wantTotal: 1438720,
			wantOK:    true,
		},
		{
			// Real values from a TrueNAS VM whose balloon driver was
			// silent for two days — the dashboard used to display
			// "138 MiB / 1.3 GiB" from them, numbers that matched
			// nothing inside the guest.
			name: "stale stats are rejected",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1355684),
				stat(memStatUsable, 1214576),
				stat(memStatLastUpdate, 1788086274),
			},
			wantStale: true,
		},
		{
			// Exactly at the threshold is still fresh; only older is stale.
			name: "stats right at the staleness limit",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1438720),
				stat(memStatUsable, 774144),
				stat(memStatLastUpdate, uint64(now.Add(-clocks.BalloonStaleAfter).Unix())),
			},
			wantUsed:  664576,
			wantTotal: 1438720,
			wantOK:    true,
		},
		{
			// Older stacks that predate the last-update tag: accept rather
			// than regress.
			name: "no last-update tag",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1438720),
				stat(memStatUsable, 774144),
			},
			wantUsed:  664576,
			wantTotal: 1438720,
			wantOK:    true,
		},
		{
			name: "zero last-update counts as no timestamp",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1438720),
				stat(memStatUsable, 774144),
				stat(memStatLastUpdate, 0),
			},
			wantUsed:  664576,
			wantTotal: 1438720,
			wantOK:    true,
		},
		{
			// A guest whose MemAvailable exceeds MemTotal gives
			// inconsistent values; fall back to MemFree rather than
			// underflow.
			name: "usable above total falls back to MemFree",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatAvailable, 1000),
				stat(memStatUsable, 2000),
				stat(memStatUnused, 300),
			},
			wantUsed:  700,
			wantTotal: 1000,
			wantOK:    true,
		},
		{
			// No MemTotal at all — a balloon driver that never reported.
			// Not stale, just unknown.
			name: "no available tag",
			stats: []libvirt.DomainMemoryStat{
				stat(memStatUnused, 300),
				stat(memStatLastUpdate, uint64(now.Add(-time.Minute).Unix())),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used, total, ok, stale := balloonMemory(tt.stats, now)
			if used != tt.wantUsed || total != tt.wantTotal || ok != tt.wantOK || stale != tt.wantStale {
				t.Errorf("balloonMemory() = (%d, %d, %v, %v), want (%d, %d, %v, %v)",
					used, total, ok, stale, tt.wantUsed, tt.wantTotal, tt.wantOK, tt.wantStale)
			}
		})
	}
}

func TestLogMemoryStaleWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	s := New(Config{Log: slog.New(slog.NewTextHandler(&buf, nil))})

	s.logMemoryStale("4_foundry", true)
	if !strings.Contains(buf.String(), "guest memory stats stale") {
		t.Fatalf("first stale observation should warn, log was: %q", buf.String())
	}

	buf.Reset()
	s.logMemoryStale("4_foundry", true)
	if buf.Len() != 0 {
		t.Fatalf("repeated stale observations should be silent, log was: %q", buf.String())
	}

	s.logMemoryStale("4_foundry", false)
	if !strings.Contains(buf.String(), "fresh again") {
		t.Fatalf("recovery should log, log was: %q", buf.String())
	}

	buf.Reset()
	s.logMemoryStale("4_foundry", false)
	if buf.Len() != 0 {
		t.Fatalf("repeated non-stale observations should be silent, log was: %q", buf.String())
	}
}

func TestNewWarnsWhenStatsPeriodNearStaleness(t *testing.T) {
	// A collection period of 3m means readings are 3–6m old at poll time,
	// while anything older than clocks.BalloonStaleAfter (5m) is rejected —
	// so the poller would reject most readings. That deserves one warning.
	tests := []struct {
		name        string
		statsPeriod time.Duration
		wantWarn    bool
	}{
		{name: "default period stays quiet", statsPeriod: clocks.DefaultStatsPeriod},
		{name: "disabled stays quiet", statsPeriod: 0},
		{name: "period near threshold warns", statsPeriod: 3 * time.Minute, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			New(Config{
				Log:         slog.New(slog.NewTextHandler(&buf, nil)),
				StatsPeriod: tt.statsPeriod,
			})
			got := strings.Contains(buf.String(), "staleness threshold")
			if got != tt.wantWarn {
				t.Fatalf("stats period %s: warned = %v, want %v (log: %q)",
					tt.statsPeriod, got, tt.wantWarn, buf.String())
			}
		})
	}
}

// agentEventMsg builds the message shape go-libvirt delivers for
// VIR_DOMAIN_EVENT_ID_AGENT_LIFECYCLE.
func agentEventMsg(id int32, state libvirt.ConnectDomainEventAgentLifecycleState) *libvirt.DomainEventCallbackAgentLifecycleMsg {
	return &libvirt.DomainEventCallbackAgentLifecycleMsg{
		Dom:   libvirt.Domain{ID: id, Name: "7_test"},
		State: int32(state),
	}
}

// waitUntil polls fn until it passes or the deadline runs out, so tests
// that hand work to a goroutine do not guess at sleep durations.
func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestHandleAgentEventHintsAndNotifies(t *testing.T) {
	var calls int32
	s := New(Config{Notify: func() { atomic.AddInt32(&calls, 1) }})

	// Two events for the same domain: the hint map must hold one entry
	// (overwrite, never accumulate) while every event gets its nudge — the
	// poll loop's debounce is what caps the rate, not this bookkeeping.
	s.handleAgentEvent(agentEventMsg(7, libvirt.ConnectDomainEventAgentLifecycleStateConnected))
	s.handleAgentEvent(agentEventMsg(7, libvirt.ConnectDomainEventAgentLifecycleStateDisconnected))

	s.mu.Lock()
	n := len(s.agentEvents)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("hint map holds %d entries, want 1 (overwritten per domain)", n)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("notify calls = %d, want 2 (one per event)", got)
	}
}

func TestHandleAgentEventWithoutNotify(t *testing.T) {
	// A Source built without Notify must not panic on an event — the drain
	// goroutine can deliver one before main wires anything, in principle.
	s := New(Config{})
	s.handleAgentEvent(agentEventMsg(3, libvirt.ConnectDomainEventAgentLifecycleStateConnected))
}

func TestConsumeAgentEventClearsHint(t *testing.T) {
	s := New(Config{})
	if _, ok := s.consumeAgentEvent(3); ok {
		t.Fatal("consume on an empty map should miss")
	}

	at := time.Now()
	s.mu.Lock()
	s.agentEvents[3] = at
	s.mu.Unlock()

	got, ok := s.consumeAgentEvent(3)
	if !ok || !got.Equal(at) {
		t.Fatalf("consume = (%v, %v), want (%v, true)", got, ok, at)
	}
	if _, ok := s.consumeAgentEvent(3); ok {
		t.Fatal("hint should be cleared after the first consume")
	}
}

func TestStartAgentEventsDeliversAndStopSilences(t *testing.T) {
	var calls int32
	s := New(Config{Notify: func() { atomic.AddInt32(&calls, 1) }})

	ch := make(chan interface{}, 4)
	s.subscribe = func(ctx context.Context, l *libvirt.Libvirt) (<-chan interface{}, error) {
		return ch, nil
	}
	s.startAgentEvents(nil)

	ch <- agentEventMsg(5, libvirt.ConnectDomainEventAgentLifecycleStateConnected)
	waitUntil(t, func() bool { return atomic.LoadInt32(&calls) == 1 })

	s.stopAgentEvents()
	// cancel() closes the context before it returns, so the drain
	// goroutine certainly sees the stop before these sleeps end; the
	// point is "no deliveries after stop", not a timing guarantee.
	time.Sleep(100 * time.Millisecond)
	ch <- agentEventMsg(5, libvirt.ConnectDomainEventAgentLifecycleStateDisconnected)
	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("notify calls = %d, want 1 — events must stop with the subscription", got)
	}
}

func TestStartAgentEventsSubscribeFailureWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	s := New(Config{
		Log:    slog.New(slog.NewTextHandler(&buf, nil)),
		Notify: func() {},
	})
	s.subscribe = func(ctx context.Context, l *libvirt.Libvirt) (<-chan interface{}, error) {
		return nil, errors.New("boom")
	}

	s.startAgentEvents(nil)
	if !strings.Contains(buf.String(), "lifecycle events unavailable") {
		t.Fatalf("first failure should warn, log was: %q", buf.String())
	}
	if s.eventCancel != nil {
		t.Fatal("a failed subscription must not leave a cancel func behind")
	}

	buf.Reset()
	s.startAgentEvents(nil)
	if buf.Len() != 0 {
		t.Fatalf("repeated failures should be silent at warn level, log was: %q", buf.String())
	}
}

func TestStartAgentEventsWithoutNotifyDoesNothing(t *testing.T) {
	var attempts int32
	s := New(Config{})
	s.subscribe = func(ctx context.Context, l *libvirt.Libvirt) (<-chan interface{}, error) {
		atomic.AddInt32(&attempts, 1)
		return make(chan interface{}), nil
	}
	s.startAgentEvents(nil)
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Fatalf("subscribe attempts = %d, want 0 — no Notify means no subscription", got)
	}
}
