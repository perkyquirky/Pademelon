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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pademelon/internal/actions"
	"pademelon/internal/clocks"
	"pademelon/internal/libvirtsrc"
	"pademelon/internal/model"
	"pademelon/internal/truenas"
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
		concurrency  = flag.Int("concurrency", clocks.DefaultConcurrency, "how many VMs to interrogate at once; 0 = one worker per VM (default)")
		theme        = flag.String("theme", web.DefaultTheme, "default colour theme: "+strings.Join(web.Themes(), ", "))
		authToken    = flag.String("auth-token", "", "token required by private routes (default: $PADAMELON_TOKEN or $PADAMELON_TOKEN_FILE)")
		allowActions = flag.Bool("allow-actions", false, "enable VM action routes (start, shutdown, reboot, force off, pause, resume); requires an auth token (default: $PADAMELON_ALLOW_ACTIONS)")
		truenasHost  = flag.String("truenas-host", "", "TrueNAS middleware websocket host, e.g. 192.168.1.100 (default: $TRUENAS_HOST; empty disables the integration)")
		truenasUser  = flag.String("truenas-user", "pademelon", "service account that owns the middleware API key (default: $TRUENAS_USER)")
		truenasKey   = flag.String("truenas-api-key", "", "middleware API key for the service account (default: $TRUENAS_API_KEY or $TRUENAS_API_KEY_FILE)")
		truenasCA    = flag.String("truenas-ca-file", "", "CA bundle to verify the middleware certificate; empty skips verification (TrueNAS ships a self-signed cert)")
		logLevel     = flag.String("log-level", "info", "debug, info, warn or error")
		logFormat    = flag.String("log-format", "text", "text or json")
		showVersion  = flag.Bool("version", false, "print version and exit")
		healthcheck  = flag.Bool("healthcheck", false, "probe a running instance and exit 0 or 1")
	)
	flag.Parse()

	// libvirt's agent and balloon-stats APIs take whole seconds. A sub-second
	// value would truncate silently (500ms -> 0). Reject it instead of
	// rounding it without telling the user. 0 is special for stats-period
	// only: it disables QEMU's collection timer entirely.
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

	// Token resolution: the first source that has a token wins. The file
	// form follows the Docker secrets convention, so the value can stay
	// out of compose files and out of `docker inspect`. A whitespace-only
	// value counts as configured-but-unset. That is a config error, not a
	// silent disable.
	token, tokenSource, err := resolveToken(*authToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pademelon: %v\n", err)
		os.Exit(2)
	}

	// The action flag also reads the environment, for compose parity with
	// the token. The binary refuses to start with actions on and no token:
	// actions are buttons that stop VMs. Such buttons never exist
	// unauthenticated, not even with a warning.
	actionsOn, err := resolveAllowActions(*allowActions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pademelon: %v\n", err)
		os.Exit(2)
	}
	if actionsOn && token == "" {
		fmt.Fprintln(os.Stderr, "pademelon: -allow-actions needs an auth token; generate one with: openssl rand -hex 32")
		os.Exit(2)
	}

	// Middleware resolution follows the same shapes as the token: flag,
	// then environment, then Docker-secrets file. A host without a key is
	// the middleware version of actions without a token: the client cannot
	// authenticate. That is a config error, not a degraded mode.
	truenasAddr := resolveTruenasHost(*truenasHost)
	truenasUsername := resolveTruenasUser(*truenasUser)
	tnKey, tnKeySource, err := resolveTruenasKey(*truenasKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pademelon: %v\n", err)
		os.Exit(2)
	}
	if truenasAddr != "" && tnKey == "" {
		fmt.Fprintln(os.Stderr, "pademelon: -truenas-host needs a middleware API key; see documentation/truenas-service-account.md")
		os.Exit(2)
	}

	if *showVersion {
		fmt.Println("pademelon", version)
		return
	}

	// The image is built FROM scratch, so there is no shell and no curl for
	// a Docker HEALTHCHECK to use. The binary probes itself instead.
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
	if actionsOn {
		log.Info("action routes enabled", "timeout", clocks.ActionTimeout.String())
	} else {
		log.Info("action routes disabled (-allow-actions is off); the dashboard is read-only")
	}

	// The middleware client owns its own connection: dial, login and
	// keepalive live on the background loop below, and the web layer only
	// ever reads its status. Requests never talk to the NAS directly.
	var truenasClient *truenas.Client
	if truenasAddr != "" {
		tn, err := truenas.New(truenas.Config{
			Host:     truenasAddr,
			Username: truenasUsername,
			APIKey:   tnKey,
			CAFile:   *truenasCA,
			Log:      log,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "pademelon: %v\n", err)
			os.Exit(2)
		}
		truenasClient = tn
		verify := "skipped — TrueNAS ships a self-signed certificate"
		if *truenasCA != "" {
			verify = "verified against " + *truenasCA
		}
		log.Info("TrueNAS middleware integration enabled",
			"host", truenasAddr, "user", truenasUsername, "key_from", tnKeySource, "certificate", verify)
	}

	cache := model.NewCache()

	// Everything that wants an early poll asks this channel: the web
	// layer's refresh button, finished action jobs, and agent lifecycle
	// events. The channel has one slot and is non-blocking: a nudge that
	// arrives while one is already pending is dropped, not queued.
	nudge := make(chan struct{}, 1)

	src := libvirtsrc.New(libvirtsrc.Config{
		Socket:       *socket,
		AgentTimeout: *agentTimeout,
		StatsPeriod:  *statsPeriod,
		Concurrency:  *concurrency,
		Log:          log,
		// Agent lifecycle events poke the same debounced channel: libvirt
		// reports a channel state change, and the poller polls early to
		// confirm it. The poller stays the only writer of the cache. The
		// event is a hint that makes the poller hurry, nothing more.
		Notify: func() {
			select {
			case nudge <- struct{}{}:
			default:
			}
		},
	})
	defer src.Close()

	// Typed-nil trap: declaring this as *actions.Store would put a nil
	// pointer inside a non-nil interface when actions are off. Web would
	// then register routes that panic on use (seen live: capabilities said
	// "actions: true" while the startup log said disabled). The interface
	// type keeps "off" a real nil.
	var actionStore web.ActionSubmitter
	var sweepStore *actions.Store // the concrete store, for the fsfreeze sweeps
	if actionsOn {
		// The middleware client joins the action store as its write
		// partner: freeze/snapshot/thaw orchestration lives in actions,
		// and the middleware call itself sits behind the interface — same
		// zone rule as the libvirt verbs. nil (integration off) keeps
		// snapshot jobs refused at submit time.
		var tnForActions actions.MiddlewareClient
		if truenasClient != nil {
			tnForActions = truenasClient
		}
		store := actions.New(actions.Config{
			Log:          log,
			Snapshot:     cache.Get,
			Conn:         src,
			Nudge:        nudge,
			AgentTimeout: *agentTimeout,
			Truenas:      tnForActions,
		})
		actionStore = store
		sweepStore = store
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if truenasClient != nil {
		go truenasClient.Run(ctx)
	}
	if sweepStore != nil {
		// The startup sweep thaws any guest found frozen outside a job —
		// the crash case that the in-memory marker cannot remember. The
		// loop sweep force-thaws tracked guests held past FreezeHoldBound.
		go sweepStore.SweepFrozenOnce(ctx)
		go sweepStore.SweepLoop(ctx)
	}

	// The typed-nil trap, third occurrence. The web and actions layers
	// have their guards; this call site is the one that panicked in
	// production. A nil *truenas.Client passed straight into the
	// middlewareSource interface is not a nil interface. pollLoop's nil
	// check would pass, and the gather would dereference a dead client.
	// Build the interface only when a real client sits behind it.
	var tnLister middlewareSource
	if truenasClient != nil {
		tnLister = truenasClient
	}
	go pollLoop(ctx, src, cache, *interval, nudge, tnLister, log)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.New(web.Config{Cache: cache, Log: log, Theme: *theme, Token: token, Nudge: nudge, Actions: actionStore, Truenas: truenasClient}).Handler(),
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

// middlewareSource is what the poll loop needs from the middleware
// client: its status and the per-dataset snapshot list. *truenas.Client
// satisfies it; a test fake can too.
type middlewareSource interface {
	Status() truenas.Status
	Snapshots(ctx context.Context, dataset string) ([]truenas.Snapshot, error)
}

// pollLoop polls straight away, then on the interval, until ctx is done.
// A nudge on the channel asks for an early poll. The refresh button,
// finished action jobs and agent lifecycle events all send nudges.
// Nudges are debounced: one that arrives sooner than clocks.NudgeInterval
// after the previous poll is dropped. A browser that refreshes rapidly,
// or an agent that starts and stops during a guest boot, therefore
// cannot flood libvirt.
//
// A failed poll is not fatal: the cache keeps the last good data and marks
// it stale, and the next tick tries to reconnect. A libvirtd restart or a
// NAS reboot should never need this container restarted. The middleware
// gather rides the same loop. Middleware trouble costs a stale snapshot
// list, never the libvirt data.
func pollLoop(ctx context.Context, src *libvirtsrc.Source, cache *model.Cache, interval time.Duration, nudge chan struct{}, tn middlewareSource, log *slog.Logger) {
	poll := func() {
		snap, err := src.Poll()
		if err != nil {
			log.Error("poll failed", "err", err)
			cache.SetError(err)
			return
		}
		if tn != nil {
			gatherTruenasSnapshots(ctx, tn, &snap, cache.Get(), log)
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
			delay := clocks.NudgeInterval - time.Since(last)
			if delay <= 0 {
				poll()
				last = time.Now()
				continue
			}
			// Too soon after the last poll — but do not lose the request
			// entirely (a finished action wants its state change
			// noticed). Re-poke when the debounce window is over.
			time.AfterFunc(delay, func() {
				select {
				case nudge <- struct{}{}:
				default:
				}
			})
		}
	}
}

// probe asks a running instance whether it is healthy. It returns a
// process exit code: 0 for healthy, 1 for not.
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

// resolveAllowActions combines the -allow-actions flag with the
// PADAMELON_ALLOW_ACTIONS environment variable, for compose parity with
// the token. A garbage value is an error, not a silent false — same
// strictness as an explicitly-empty token.
func resolveAllowActions(flagValue bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv("PADAMELON_ALLOW_ACTIONS"))
	if raw == "" {
		return flagValue, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("PADAMELON_ALLOW_ACTIONS is %q, want a boolean (true/false/1/0)", raw)
	}
	return v, nil
}

// gatherTruenasSnapshots asks the middleware for each VM's disk-dataset
// snapshots and pours them into the snapshot that the poll loop is about
// to cache. File-backed disks have no zvol and are skipped. A gather
// failure costs the freshness of the list, never the libvirt data. The
// previous poll's lists are carried over, so a middleware hiccup reads
// as "stale", not as "your snapshots vanished".
func gatherTruenasSnapshots(ctx context.Context, tn middlewareSource, snap *model.Snapshot, prev model.Snapshot, log *slog.Logger) {
	// A second guard behind the call-site check: a typed-nil client
	// inside the interface (the crash from 2026-09-07) must read as
	// "integration off", not as a SIGSEGV mid-poll.
	if tn == nil || reflect.ValueOf(tn).Kind() == reflect.Ptr && reflect.ValueOf(tn).IsNil() {
		return
	}

	gctx, cancel := context.WithTimeout(ctx, clocks.SnapshotGatherBudget)
	defer cancel()

	st := tn.Status()
	g := &model.TruenasGather{Connected: st.Connected, Version: st.Version, At: time.Now()}

	for i := range snap.VMs {
		vm := &snap.VMs[i]
		outOfBudget := false
		for _, disk := range vm.Disks {
			if gctx.Err() != nil {
				outOfBudget = true
				break
			}
			ds, ok := truenas.DatasetFromDiskSource(disk.Source)
			if !ok {
				continue // file-backed disk, ISO, or a source with no mapping
			}
			list, err := tn.Snapshots(gctx, ds)
			if err != nil {
				if g.Error == "" {
					g.Error = err.Error()
				}
				continue
			}
			for _, s := range list {
				vm.Snapshots = append(vm.Snapshots, model.ZfsSnapshot{
					ID: s.ID, Dataset: s.Dataset, Name: s.Name,
					Created: s.Created, Used: s.Used, Referenced: s.Referenced,
				})
			}
		}
		sort.Slice(vm.Snapshots, func(a, b int) bool {
			return vm.Snapshots[a].Created > vm.Snapshots[b].Created
		})
		// Carry the previous list over when this round came up empty for
		// a VM that had one — middleware down should look stale, not
		// empty. A VM with genuinely no snapshots stays empty either way.
		if len(vm.Snapshots) == 0 && (outOfBudget || g.Error != "") {
			for _, pv := range prev.VMs {
				if pv.Domain == vm.Domain && len(pv.Snapshots) > 0 {
					vm.Snapshots = pv.Snapshots
					break
				}
			}
		}
		if outOfBudget {
			log.Warn("snapshot gather ran out of budget; the next poll finishes the job")
			break
		}
	}
	snap.Truenas = g
}

// resolveTruenasHost picks the middleware address: the flag, then
// $TRUENAS_HOST. Empty means the integration is off — the only soft
// outcome in the middleware config, because "off" is a legitimate choice
// while "half-configured" is not.
func resolveTruenasHost(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return strings.TrimSpace(os.Getenv("TRUENAS_HOST"))
}

// resolveTruenasUser picks the service account name: flag, then
// $TRUENAS_USER, then the documented default.
func resolveTruenasUser(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := strings.TrimSpace(os.Getenv("TRUENAS_USER")); env != "" {
		return env
	}
	return "pademelon"
}

// resolveTruenasKey picks the middleware API key the same way the auth
// token resolves: flag, then $TRUENAS_API_KEY, then the file named by
// $TRUENAS_API_KEY_FILE (the Docker-secrets convention). The source is
// for the startup log only; the value is never logged. Configured but
// empty is an error — the same strictness as the auth token.
func resolveTruenasKey(flagValue string) (key, source string, err error) {
	switch {
	case flagValue != "":
		key, source = flagValue, "flag"
	case os.Getenv("TRUENAS_API_KEY") != "":
		key, source = os.Getenv("TRUENAS_API_KEY"), "environment"
	case os.Getenv("TRUENAS_API_KEY_FILE") != "":
		path := os.Getenv("TRUENAS_API_KEY_FILE")
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("reading TRUENAS_API_KEY_FILE: %w", err)
		}
		key, source = strings.TrimSpace(string(raw)), "file "+path
	}
	if strings.TrimSpace(key) == "" && (flagValue != "" ||
		os.Getenv("TRUENAS_API_KEY") != "" || os.Getenv("TRUENAS_API_KEY_FILE") != "") {
		return "", "", fmt.Errorf("middleware API key is configured but empty; see documentation/truenas-service-account.md")
	}
	return key, source, nil
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
