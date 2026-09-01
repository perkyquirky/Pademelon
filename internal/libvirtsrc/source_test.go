package libvirtsrc

import (
	"bytes"
	"log/slog"
	"strings"
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
			// Real values from a TrueNAS VM whose balloon driver had been
			// silent for two days — these used to display as a confident
			// "138 MiB / 1.3 GiB" that matched nothing inside the guest.
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
			// A guest that reports MemAvailable above MemTotal is talking
			// nonsense; fall back to MemFree rather than underflow.
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
	// so most readings would never survive. That deserves one warning.
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
