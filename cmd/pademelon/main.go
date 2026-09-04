// Command pademelon serves a dashboard of the VMs running on a
// TrueNAS Scale host.
//
// It talks to libvirt over the host's unix socket and to each VM's QEMU guest
// agent through libvirt.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pademelon/internal/clocks"
	"pademelon/internal/libvirtsrc"
	"pademelon/internal/model"
	"pademelon/internal/web"
)

// version is stamped in at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		listen       = flag.String("listen", ":8088", "address to serve the dashboard on")
		socket       = flag.String("socket", "/run/truenas_libvirt/libvirt-sock", "path to the libvirt unix socket")
		interval     = flag.Duration("interval", clocks.DefaultPollInterval, "how often to poll libvirt")
		agentTimeout = flag.Duration("agent-timeout", clocks.DefaultAgentTimeout, "how long to allow one guest agent command, e.g. 5s")
		statsPeriod  = flag.Duration("stats-period", clocks.DefaultStatsPeriod, "how often QEMU refreshes guest balloon stats; 0s shows allocated RAM only")
		concurrency  = flag.Int("concurrency", 8, "how many VMs to interrogate at once")
		theme        = flag.String("theme", web.DefaultTheme, "default colour theme: "+strings.Join(web.Themes(), ", "))
		authToken    = flag.String("auth-token", "", "token required by private routes (default: $PADAMELON_TOKEN or $PADAMELON_TOKEN_FILE)")
		logLevel     = flag.String("log-level", "info", "debug, info, warn or error")
		logFormat    = flag.String("log-format", "text", "text or json")
		showVersion  = flag.Bool("version", false, "print version and exit")
		healthcheck  = flag.Bool("healthcheck", false, "probe a running instance and exit 0 or 1")
	)
	flag.Parse()

	// libvirt's agent and balloon-stats APIs take whole seconds. A sub-second
	// value would silently truncate (500ms -> 0), so reject it instead of
	// rounding behind the user's back. 0 is special for stats-period only:
	// it disables QEMU's collection timer entirely.
	if *agentTimeout < time.Second {
		fmt.Fprintf(os.Stderr, "pademelon: -agent-timeout must be at least 1s, got %s\n", *agentTimeout)
		os.Exit(2)
	}
	if *statsPeriod != 0 && *statsPeriod < time.Second {
		fmt.Fprintf(os.Stderr, "pademelon: -stats-period must be 0s (disabled) or at least 1s, got %s\n", *statsPeriod)
		os.Exit(2)
	}
	if !web.ValidTheme(*theme) {
		fmt.Fprintf(os.Stderr, "pademelon: unknown theme %q, valid themes: %s\n", *theme, strings.Join(web.Themes(), ", "))
		os.Exit(2)
	}

	// Token resolution: first present wins. The file form follows the Docker
	// secrets convention so the value can stay out of compose files and out
	// of `docker inspect`. Whitespace-only counts as unset-but-configured,
	// which is a config error rather than a silent disable.
	token, tokenSource, err := resolveToken(*authToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pademelon: %v\n", err)
		os.Exit(2)
	}

	if *showVersion {
		fmt.Println("pademelon", version)
		return
	}

	// The image is built FROM scratch, so there's no shell and no curl for a
	// Docker HEALTHCHECK to use. The binary probes itself instead.
	if *healthcheck {
		os.Exit(probe(*listen))
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pademelon:", err)
		os.Exit(2)
	}
	slog.SetDefault(log)

	log.Info("starting pademelon",
		"version", version,
		"listen", *listen,
		"socket", *socket,
		"interval", *interval,
		"agent_timeout_s", int64(*agentTimeout/time.Second),
		"stats_period_s", int64(*statsPeriod/time.Second),
	)
	if token == "" {
		log.Warn("auth disabled — the dashboard is open to anyone who can reach the listen address")
	} else {
		log.Info("auth enabled; private routes require the token", "token_from", tokenSource, "cookie_days", int64(clocks.SessionCookieMaxAge/(24*time.Hour)))
	}

	cache := model.NewCache()

	// The web layer can nudge the poll loop for an early poll (refresh
	// button, later the post-action poke). One slot, non-blocking: a nudge
	// arriving while one is already pending is dropped, not queued.
	nudge := make(chan struct{}, 1)

	src := libvirtsrc.New(libvirtsrc.Config{
		Socket:       *socket,
		AgentTimeout: *agentTimeout,
		StatsPeriod:  *statsPeriod,
		Concurrency:  *concurrency,
		Log:          log,
	})
	defer src.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pollLoop(ctx, src, cache, *interval, nudge, log)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.New(cache, log, *theme, token, nudge).Handler(),
		ReadHeaderTimeout: clocks.HeaderReadTimeout,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), clocks.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// pollLoop polls straight away, then on the interval, until ctx is done.
// A nudge on the channel asks for an early poll — the refresh button uses
// it, and later phases will too. Nudges are debounced: one arriving sooner
// than clocks.NudgeInterval after the previous poll is dropped, so a
// browser hammering the refresh route can't hammer libvirt.
//
// A failed poll is not fatal: the cache keeps the last good data and marks it
// stale, and the next tick tries to reconnect. libvirtd restarting or the NAS
// rebooting should never need this container restarted.
func pollLoop(ctx context.Context, src *libvirtsrc.Source, cache *model.Cache, interval time.Duration, nudge <-chan struct{}, log *slog.Logger) {
	poll := func() {
		snap, err := src.Poll()
		if err != nil {
			log.Error("poll failed", "err", err)
			cache.SetError(err)
			return
		}
		cache.Set(snap)
	}

	poll()
	last := time.Now()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
			last = time.Now()
		case <-nudge:
			if time.Since(last) < clocks.NudgeInterval {
				continue
			}
			poll()
			last = time.Now()
		}
	}
}

// probe asks a running instance whether it's healthy. Returns a process exit
// code: 0 healthy, 1 not.
func probe(listen string) int {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pademelon: bad listen address:", err)
		return 1
	}
	// A listen address of ":8088" or "0.0.0.0:8088" means every interface;
	// dial back through loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: clocks.ProbeTimeout}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pademelon:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "pademelon: unhealthy, HTTP", resp.StatusCode)
		return 1
	}
	return 0
}

// resolveToken picks the auth token from the first place that has one:
// the -auth-token flag, then $PADAMELON_TOKEN, then the file named by
// $PADAMELON_TOKEN_FILE (the Docker-secrets convention). The returned source
// name is for the startup log only; the value is never logged. An explicitly
// configured but empty value is an error — a typo should not silently
// disable auth.
func resolveToken(flagValue string) (token, source string, err error) {
	switch {
	case flagValue != "":
		token, source = flagValue, "flag"
	case os.Getenv("PADAMELON_TOKEN") != "":
		token, source = os.Getenv("PADAMELON_TOKEN"), "environment"
	case os.Getenv("PADAMELON_TOKEN_FILE") != "":
		path := os.Getenv("PADAMELON_TOKEN_FILE")
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("reading PADAMELON_TOKEN_FILE: %w", err)
		}
		token, source = strings.TrimSpace(string(raw)), "file "+path
	}
	if strings.TrimSpace(token) == "" && (flagValue != "" ||
		os.Getenv("PADAMELON_TOKEN") != "" || os.Getenv("PADAMELON_TOKEN_FILE") != "") {
		return "", "", fmt.Errorf("auth token is configured but empty; generate one with: openssl rand -hex 32")
	}
	return token, source, nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "info":
		lv = slog.LevelInfo
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}
