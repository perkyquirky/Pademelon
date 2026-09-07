package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pademelon/internal/model"
	"pademelon/internal/truenas"
)

// TestResolveTokenPrecedence pins the resolution order: flag, then
// $PADAMELON_TOKEN, then the file named by $PADAMELON_TOKEN_FILE.
func TestResolveTokenPrecedence(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		flagValue string
		env       string
		file      string
		want      string
	}{
		{name: "flag wins", flagValue: "from-flag", env: "from-env", file: tokenFile, want: "from-flag"},
		{name: "env over file", env: "from-env", file: tokenFile, want: "from-env"},
		{name: "file last", file: tokenFile, want: "from-file"},
		{name: "nothing set", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADAMELON_TOKEN", tc.env)
			t.Setenv("PADAMELON_TOKEN_FILE", tc.file)
			got, source, err := resolveToken(tc.flagValue)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
			switch {
			case tc.flagValue != "":
				if source != "flag" {
					t.Errorf("source = %q, want flag", source)
				}
			case tc.env != "":
				if source != "environment" {
					t.Errorf("source = %q, want environment", source)
				}
			case tc.file != "":
				if source != "file "+tokenFile {
					t.Errorf("source = %q, want file path", source)
				}
			}
		})
	}
}

// TestResolveTokenFileTrimsWhitespace covers the Docker-secrets case: the
// file conventionally ends with a newline, which must not become part of
// the token.
func TestResolveTokenFileTrimsWhitespace(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  secret-token  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADAMELON_TOKEN", "")
	t.Setenv("PADAMELON_TOKEN_FILE", tokenFile)
	got, _, err := resolveToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "secret-token" {
		t.Errorf("token = %q, want %q (trimmed)", got, "secret-token")
	}
}

// TestResolveTokenRejectsEmpty ensures a configured-but-empty value is an
// error, not a silent disable of auth.
func TestResolveTokenRejectsEmpty(t *testing.T) {
	cases := []struct {
		name      string
		flagValue string
		env       string
		file      string
	}{
		{name: "whitespace flag", flagValue: "   "},
		{name: "whitespace env", env: "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADAMELON_TOKEN", tc.env)
			t.Setenv("PADAMELON_TOKEN_FILE", tc.file)
			_, _, err := resolveToken(tc.flagValue)
			if err == nil {
				t.Error("empty configured token should be an error")
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("PADAMELON_TOKEN", "")
		t.Setenv("PADAMELON_TOKEN_FILE", filepath.Join(t.TempDir(), "nope"))
		if _, _, err := resolveToken(""); err == nil {
			t.Error("unreadable token file should be an error")
		}
	})
}

// TestResolveAllowActions pins the flag+env combination for the action
// switch: the flag is the default, the environment overrides, and a
// garbage value is an error rather than a silent false. The
// refuses-without-token rule lives in main and is exercised by the live
// checklist, since it is two lines of os.Exit.
func TestResolveAllowActions(t *testing.T) {
	tests := []struct {
		name    string
		flag    bool
		env     string
		want    bool
		wantErr bool
	}{
		{name: "flag off, nothing set", want: false},
		{name: "flag on, nothing set", flag: true, want: true},
		{name: "env overrides a false flag", env: "1", want: true},
		{name: "env false beats a true flag", flag: true, env: "false", want: false},
		{name: "garbage env is an error", env: "yes-please", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADAMELON_ALLOW_ACTIONS", tc.env)
			got, err := resolveAllowActions(tc.flag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("resolveAllowActions = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveTruenasKeyPrecedence pins the middleware key order: flag,
// then $TRUENAS_API_KEY, then the file named by $TRUENAS_API_KEY_FILE.
func TestResolveTruenasKeyPrecedence(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		flagValue string
		env       string
		file      string
		want      string
	}{
		{name: "flag wins", flagValue: "from-flag", env: "from-env", file: keyFile, want: "from-flag"},
		{name: "env over file", env: "from-env", file: keyFile, want: "from-env"},
		{name: "file last", file: keyFile, want: "from-file"},
		{name: "nothing set", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUENAS_API_KEY", tc.env)
			t.Setenv("TRUENAS_API_KEY_FILE", tc.file)
			got, _, err := resolveTruenasKey(tc.flagValue)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveTruenasKeyRejectsEmpty covers the configured-but-empty case:
// a typo must be an error, never a silent integration-off.
func TestResolveTruenasKeyRejectsEmpty(t *testing.T) {
	t.Setenv("TRUENAS_API_KEY", "   ")
	if _, _, err := resolveTruenasKey(""); err == nil {
		t.Fatal("whitespace-only key resolved cleanly; it must be an error")
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRUENAS_API_KEY", "")
	t.Setenv("TRUENAS_API_KEY_FILE", keyFile)
	if _, _, err := resolveTruenasKey(""); err == nil {
		t.Fatal("empty key file resolved cleanly; it must be an error")
	}
}

// TestResolveTruenasHostAndUser covers the smaller settings: host falls
// back to $TRUENAS_HOST, user to $TRUENAS_USER, user defaults to pademelon.
func TestResolveTruenasHostAndUser(t *testing.T) {
	if got := resolveTruenasHost("flag-host"); got != "flag-host" {
		t.Errorf("host flag ignored: %q", got)
	}
	t.Setenv("TRUENAS_HOST", "env-host")
	if got := resolveTruenasHost(""); got != "env-host" {
		t.Errorf("host env ignored: %q", got)
	}
	t.Setenv("TRUENAS_HOST", "")
	if got := resolveTruenasHost(""); got != "" {
		t.Errorf("integration should be off with nothing set, got %q", got)
	}

	t.Setenv("TRUENAS_USER", "env-user")
	if got := resolveTruenasUser(""); got != "env-user" {
		t.Errorf("user env ignored: %q", got)
	}
	t.Setenv("TRUENAS_USER", "")
	if got := resolveTruenasUser(""); got != "pademelon" {
		t.Errorf("user default = %q, want pademelon", got)
	}
}

// fakeLister is the middlewareSource surface the gather needs, canned.
type fakeLister struct {
	st    truenas.Status
	lists map[string][]truenas.Snapshot
	err   error
}

func (f *fakeLister) Status() truenas.Status { return f.st }
func (f *fakeLister) Snapshots(_ context.Context, dataset string) ([]truenas.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.lists[dataset], nil
}

// TestGatherSnapshots covers the pour: per-VM dataset mapping (zvol in,
// file-backed out), newest-first sort, and the middleware status block.
func TestGatherSnapshots(t *testing.T) {
	fl := &fakeLister{
		st: truenas.Status{Connected: true, Version: "TrueNAS-25.10.5"},
		lists: map[string][]truenas.Snapshot{
			"nvme/vms/alpine_test-bxuwle": {
				{ID: "d@old", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "old", Created: 100},
				{ID: "d@new", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "new", Created: 200},
			},
		},
	}
	snap := model.Snapshot{VMs: []model.VM{{
		Domain: "14_alpine_test",
		Disks: []model.Disk{
			{Dev: "vda", Source: "/dev/zvol/nvme/vms/alpine_test-bxuwle"},
			{Dev: "vdb", Source: "/mnt/pool/disks/file.qcow2"}, // not a zvol: skipped
		},
	}}}

	gatherTruenasSnapshots(context.Background(), fl, &snap, model.Snapshot{}, slog.Default())

	vm := snap.VMs[0]
	if len(vm.Snapshots) != 2 {
		t.Fatalf("gathered %d snapshots, want 2 (the file-backed disk must be skipped)", len(vm.Snapshots))
	}
	if vm.Snapshots[0].Name != "new" || vm.Snapshots[1].Name != "old" {
		t.Errorf("snapshots not newest-first: %s then %s", vm.Snapshots[0].Name, vm.Snapshots[1].Name)
	}
	if snap.Truenas == nil || !snap.Truenas.Connected || snap.Truenas.Version != "TrueNAS-25.10.5" {
		t.Errorf("truenas gather block = %+v", snap.Truenas)
	}
}

// TestGatherCarriesOverOnFailure keeps a middleware hiccup from reading
// as "your snapshots vanished": when a round comes up empty for a VM that
// had a list, the previous poll's list rides along and the error says why.
func TestGatherCarriesOverOnFailure(t *testing.T) {
	fl := &fakeLister{st: truenas.Status{Connected: false}, err: errors.New("middleware not connected")}
	snap := model.Snapshot{VMs: []model.VM{{
		Domain: "14_alpine_test",
		Disks:  []model.Disk{{Dev: "vda", Source: "/dev/zvol/nvme/vms/alpine_test-bxuwle"}},
	}}}
	prev := model.Snapshot{VMs: []model.VM{{
		Domain:    "14_alpine_test",
		Snapshots: []model.ZfsSnapshot{{ID: "d@yesterday", Created: 50}},
	}}}

	gatherTruenasSnapshots(context.Background(), fl, &snap, prev, slog.Default())

	if len(snap.VMs[0].Snapshots) != 1 || snap.VMs[0].Snapshots[0].ID != "d@yesterday" {
		t.Errorf("carry-over failed: %+v", snap.VMs[0].Snapshots)
	}
	if snap.Truenas.Error == "" {
		t.Error("the gather error should be recorded, not swallowed")
	}
}

// TestGatherBudgetCutsTheRoundOff: a gather that would outlive its
// context stops and reports, instead of stretching one poll across
// minutes of unreachable middleware.
func TestGatherBudgetCutsTheRoundOff(t *testing.T) {
	slow := &slowLister{delay: 100 * time.Millisecond}
	snap := model.Snapshot{VMs: []model.VM{{
		Domain: "14_alpine_test",
		Disks:  []model.Disk{{Dev: "vda", Source: "/dev/zvol/nvme/vms/x"}},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	gatherTruenasSnapshots(ctx, slow, &snap, model.Snapshot{}, slog.Default())
	if snap.Truenas == nil || snap.Truenas.Error == "" {
		t.Error("the budget overrun should be recorded in the gather error")
	}
}

// slowLister sleeps past any short budget, simulating a stuck middleware.
type slowLister struct{ delay time.Duration }

func (s *slowLister) Status() truenas.Status { return truenas.Status{Connected: true} }
func (s *slowLister) Snapshots(ctx context.Context, _ string) ([]truenas.Snapshot, error) {
	select {
	case <-time.After(s.delay):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
