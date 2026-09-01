// Command pademelon serves a read-only dashboard of the VMs running on a
// TrueNAS Scale host.
//
// It talks to libvirt over the host's unix socket and to each VM's QEMU guest
// agent through libvirt. It never changes anything.
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
	"syscall"
	"time"

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
		interval     = flag.Duration("interval", 30*time.Second, "how often to poll libvirt")
		agentTimeout = flag.Int("agent-timeout", 5, "seconds to allow a guest agent command")
		statsPeriod  = flag.Int("stats-period", 10, "seconds between guest balloon stat refreshes; 0 shows allocated RAM only")
		concurrency  = flag.Int("concurrency", 8, "how many VMs to interrogate at once")
		logLevel     = flag.String("log-level", "info", "debug, info, warn or error")
		logFormat    = flag.String("log-format", "text", "text or json")
		showVersion  = flag.Bool("version", false, "print version and exit")
		healthcheck  = flag.Bool("healthcheck", false, "probe a running instance and exit 0 or 1")
	)
	flag.Parse()

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
		"agent_timeout_s", *agentTimeout,
		"stats_period_s", *statsPeriod,
	)

	cache := model.NewCache()

	src := libvirtsrc.New(libvirtsrc.Config{
		Socket:       *socket,
		AgentTimeout: int32(*agentTimeout),
		StatsPeriod:  int32(*statsPeriod),
		Concurrency:  *concurrency,
		Log:          log,
	})
	defer src.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pollLoop(ctx, src, cache, *interval, log)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.New(cache, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// pollLoop polls straight away, then on the interval, until ctx is done.
//
// A failed poll is not fatal: the cache keeps the last good data and marks it
// stale, and the next tick tries to reconnect. libvirtd restarting or the NAS
// rebooting should never need this container restarted.
func pollLoop(ctx context.Context, src *libvirtsrc.Source, cache *model.Cache, interval time.Duration, log *slog.Logger) {
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

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
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

	client := &http.Client{Timeout: 3 * time.Second}
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
