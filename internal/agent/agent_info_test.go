package agent

import (
	"testing"
)

func TestInfoReturnsVersionAndCommands(t *testing.T) {
	reply := `{"return":{"version":"8.2","supported_commands":[
{"enabled":true,"name":"guest-ping","success-response":true},
{"enabled":true,"name":"guest-shutdown","success-response":true},
{"enabled":true,"name":"guest-get-time","success-response":true}]}}`
	version, commands, err := Info(staticCaller(reply))
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if version != "8.2" {
		t.Errorf("version = %q, want 8.2", version)
	}
	if len(commands) != 3 || commands[0] != "guest-ping" || commands[2] != "guest-get-time" {
		t.Errorf("commands = %v, want the three guest-* names", commands)
	}

	// An agent too old for the command answers with an error envelope,
	// not an empty everything — the caller needs to see that as an error
	// so the UI can show a dash instead of a made-up blank.
	tooOld := `{"error":{"class":"CommandNotFound","desc":"The command guest-info has not been found"}}`
	if _, _, err := Info(staticCaller(tooOld)); err == nil {
		t.Error("Info should error on an error envelope")
	}
}

func TestTimeReturnsGuestClock(t *testing.T) {
	// 2026-09-01 00:00:00.123456789 UTC. The reply is the bare number —
	// the first guess wrapped it in an object and a real QGA 11 agent
	// refused to decode it, which is what the live test is for.
	reply := `{"return":1788220800123456789}`
	got, err := Time(staticCaller(reply))
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	if got != 1788220800123456789 {
		t.Errorf("Time = %d, want 1788220800123456789", got)
	}

	bad := `{"error":{"class":"CommandNotFound","desc":"The command guest-get-time has not been found"}}`
	if _, err := Time(staticCaller(bad)); err == nil {
		t.Error("Time should error on an error envelope")
	}
}
