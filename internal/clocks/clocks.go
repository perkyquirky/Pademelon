// Package clocks is the single source of truth for every timer Pademelon
// runs on. The "Timers and refresh rates" table in README2.md is the human
// view of everything here; add a new clock to both or neither.
//
// There are two families:
//
//   - Freshness knobs control how up to date the data is. They are
//     user-facing and flag-configurable; the constants here are the flag
//     defaults.
//   - Failure bounds cap how long one bad thing can stall the process.
//     They are operational constants, deliberately not flags.
package clocks

import "time"

// Freshness defaults. Each backs a flag: -interval, -agent-timeout and
// -stats-period.
const (
	// DefaultPollInterval is how often the poller queries libvirt. It is
	// also the CPU sampling window — CPU% is measured across consecutive
	// polls, so the first poll always says "measuring…".
	DefaultPollInterval = 30 * time.Second

	// DefaultAgentTimeout is how long one guest agent command may take.
	DefaultAgentTimeout = 5 * time.Second

	// DefaultStatsPeriod is how often QEMU re-collects balloon stats inside
	// each guest. 0 disables the collection timer.
	DefaultStatsPeriod = 10 * time.Second
)

// UIRefresh is how often the browser re-fetches /api/vms. The real value
// lives as REFRESH_MS in internal/web/index.html — the page is embedded
// statically and cannot read Go constants — so this constant mirrors it and
// TestUIRefreshMatchesEmbeddedHTML in the web package fails if the two drift
// apart.
//
// It should stay far below DefaultPollInterval: the page re-reads cached
// data, so refreshing faster than the poll just makes the UI feel live.
const UIRefresh = 1500 * time.Millisecond

// BalloonStaleAfter is how old balloon stats may be before libvirtsrc stops
// trusting them and falls back to "allocated". A live guest refreshes them
// on every query, so anything much older means the virtio_balloon driver in
// the guest has gone quiet and QEMU is handing back a fossil.
//
// It must stay comfortably above the stats period: at poll time a reading is
// roughly one to two collection periods old, so libvirtsrc.New warns at
// startup when -stats-period gets within 2× of this threshold.
const BalloonStaleAfter = 5 * time.Minute

// Failure bounds — deliberately not flags. One comment each, saying what
// breaks if the value moves.
const (
	// ProbeTimeout is the HTTP client timeout for the binary's self-probe
	// (the Dockerfile's HEALTHCHECK runs `/pademelon -healthcheck`). It must
	// stay shorter than the HEALTHCHECK --timeout in the Dockerfile, or
	// Docker starts killing checks that would have passed.
	// TestDockerfileHealthcheckTimeoutExceedsProbe in cmd/pademelon
	// enforces that ordering.
	ProbeTimeout = 3 * time.Second

	// HeaderReadTimeout caps how long the HTTP server waits for request
	// headers, so a slow client can't hold a connection open forever.
	HeaderReadTimeout = 10 * time.Second

	// ShutdownTimeout is how long graceful shutdown waits for in-flight
	// requests before walking away.
	ShutdownTimeout = 5 * time.Second
)
