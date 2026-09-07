package truenas

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveSnapshots runs against a real NAS when the environment points at
// one — the wire-format check the in-process fake can only approximate.
// CI skips it (no TRUENAS_HOST); locally:
//
//	TRUENAS_HOST=192.168.1.100 TRUENAS_API_KEY_FILE=.truenas-api-key \
//	  go test ./internal/truenas/ -run TestLiveSnapshots -v
//
// TRUENAS_TEST_DATASET overrides the dataset if the scratch VM changes.
func TestLiveSnapshots(t *testing.T) {
	host := os.Getenv("TRUENAS_HOST")
	if host == "" {
		t.Skip("TRUENAS_HOST unset — live test skipped")
	}
	key := os.Getenv("TRUENAS_API_KEY")
	if key == "" {
		if path := os.Getenv("TRUENAS_API_KEY_FILE"); path != "" {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading TRUENAS_API_KEY_FILE: %v", err)
			}
			key = strings.TrimSpace(string(raw))
		}
	}
	if key == "" {
		t.Skip("no middleware key in TRUENAS_API_KEY or TRUENAS_API_KEY_FILE — live test skipped")
	}
	dataset := os.Getenv("TRUENAS_TEST_DATASET")
	if dataset == "" {
		dataset = "nvme/vms/alpine_test-bxuwle"
	}

	c, err := New(Config{
		Host:     host,
		Username: "pademelon",
		APIKey:   key,
		Timeout:  10 * time.Second,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c.Status().Connected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !c.Status().Connected {
		t.Fatalf("never connected: %+v", c.Status())
	}

	list, err := c.Snapshots(ctx, dataset)
	if err != nil {
		t.Fatalf("Snapshots(%s): %v", dataset, err)
	}
	t.Logf("dataset %s: %d snapshot(s)", dataset, len(list))
	for i, s := range list {
		if i >= 5 {
			t.Logf("  … and %d more", len(list)-5)
			break
		}
		t.Logf("  %s created=%d used=%d referenced=%d", s.Name, s.Created, s.Used, s.Referenced)
	}
	for i, s := range list {
		if s.ID == "" || !strings.Contains(s.ID, "@") {
			t.Errorf("snapshot %d has a malformed id %q", i, s.ID)
		}
		if s.Created <= 0 {
			t.Errorf("snapshot %s has no creation timestamp", s.ID)
		}
	}
}

// TestLiveCreateSnapshot is the real write cycle: create through
// Pademelon's client, see it in the query, delete it again. The same
// class of live test that the recon script ran by hand, this time through
// the shipped code path. Skipped without TRUENAS_HOST, like the list
// test above.
func TestLiveCreateSnapshot(t *testing.T) {
	host := os.Getenv("TRUENAS_HOST")
	if host == "" {
		t.Skip("TRUENAS_HOST unset — live test skipped")
	}
	key := os.Getenv("TRUENAS_API_KEY")
	if key == "" {
		if path := os.Getenv("TRUENAS_API_KEY_FILE"); path != "" {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading TRUENAS_API_KEY_FILE: %v", err)
			}
			key = strings.TrimSpace(string(raw))
		}
	}
	if key == "" {
		t.Skip("no middleware key — live test skipped")
	}
	dataset := os.Getenv("TRUENAS_TEST_DATASET")
	if dataset == "" {
		dataset = "nvme/vms/alpine_test-bxuwle"
	}

	c, err := New(Config{
		Host:     host,
		Username: "pademelon",
		APIKey:   key,
		Timeout:  10 * time.Second,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c.Status().Connected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !c.Status().Connected {
		t.Fatalf("never connected: %+v", c.Status())
	}

	name := "pademelon-live-test-" + time.Now().Format("2006-01-02_15-04-05")
	id, err := c.CreateSnapshot(ctx, dataset, name)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Logf("created %s", id)

	list, err := c.Snapshots(ctx, dataset)
	if err != nil {
		t.Fatalf("Snapshots after create: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("created snapshot %s not in the query reply (%d snapshots)", id, len(list))
	}

	if _, err := c.Call(ctx, "pool.snapshot.delete", id); err != nil {
		t.Fatalf("cleanup delete of %s failed — remove it in the TrueNAS UI: %v", id, err)
	}
	list, err = c.Snapshots(ctx, dataset)
	if err != nil {
		t.Fatalf("Snapshots after delete: %v", err)
	}
	for _, s := range list {
		if s.ID == id {
			t.Errorf("snapshot %s still listed after delete", id)
		}
	}
	t.Logf("create/query/delete cycle complete on %s", dataset)
}
