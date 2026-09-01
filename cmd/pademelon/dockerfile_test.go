package main

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"pademelon/internal/clocks"
)

// TestDockerfileHealthcheckTimeoutExceedsProbe keeps the Dockerfile's
// HEALTHCHECK and clocks.ProbeTimeout from drifting apart: the self-probe
// must give up before Docker kills the check, or Docker starts failing
// checks that would have passed. See clocks.ProbeTimeout.
func TestDockerfileHealthcheckTimeoutExceedsProbe(t *testing.T) {
	data, err := os.ReadFile("../../Dockerfile")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("Dockerfile not present (binary run outside the repo)")
	}
	if err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile(`HEALTHCHECK[^\n]*--timeout=(\d+)s`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatal("no HEALTHCHECK --timeout found in Dockerfile")
	}
	secs, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("bad HEALTHCHECK --timeout in Dockerfile: %v", err)
	}

	timeout := time.Duration(secs) * time.Second
	if timeout <= clocks.ProbeTimeout {
		t.Fatalf("Dockerfile HEALTHCHECK --timeout (%s) must be greater than clocks.ProbeTimeout (%s), or Docker kills checks that would have passed",
			timeout, clocks.ProbeTimeout)
	}
}
