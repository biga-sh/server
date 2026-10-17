// Command biga-server runs the biga session server.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/config"
	"github.com/biga-sh/server/internal/events"
	"github.com/biga-sh/server/internal/httpapi"
	"github.com/biga-sh/server/internal/ratelimit"
	"github.com/biga-sh/server/internal/service"
	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/version"
)

const usage = `biga-server runs the biga session server.

Usage:
  biga-server [serve]      Serve the API (the default)
  biga-server migrate      Apply database migrations and exit
  biga-server cleanup      Run the retention sweep once and exit
  biga-server healthcheck  Exit 0 if the local server answers /healthz
  biga-server gen-key      Print a new access key for BIGA_ACCESS_KEYS
  biga-server version      Print the version

Configuration is read from the environment; see the README.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = withConfig(func(cfg config.Config, log *slog.Logger) error { return serve(ctx, cfg, log) })
	case "migrate":
		err = withConfig(func(cfg config.Config, log *slog.Logger) error {
			return store.Migrate(ctx, cfg.DatabaseURL, log)
		})
	case "cleanup":
		err = withConfig(func(cfg config.Config, log *slog.Logger) error { return cleanup(ctx, cfg, log, stdout) })
	case "healthcheck":
		err = healthcheck(ctx, args)
	case "gen-key":
		_, err = fmt.Fprintln(stdout, auth.NewKey())
	case "version", "--version", "-v":
		_, err = fmt.Fprintln(stdout, version.String())
	case "help", "--help", "-h":
		_, err = fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "biga-server:", err)
		return 1
	}
	return 0
}

func withConfig(fn func(config.Config, *slog.Logger) error) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return fn(cfg, slog.New(h))
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	log.Info("starting biga-server", "version", version.String())
	openCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	st, err := store.Open(openCtx, cfg.DatabaseURL, log)
	cancel()
	if err != nil {
		return err
	}
	defer st.Close()
	if cfg.MigrateOnStart {
		if err := store.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
			return err
		}
	}

	var (
		rdb     *redis.Client
		bridge  events.Bridge
		limiter ratelimit.Limiter = ratelimit.NewMemory()
	)
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(opt)
		defer rdb.Close()
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			log.Warn("Redis is not answering yet; live events stay on this instance until it does", "error", err)
		}
		cancel()
		bridge, limiter = events.NewRedisBridge(rdb, log), ratelimit.NewRedis(rdb, log)
	}

	hub := events.NewHub(bridge, log)
	svc := service.New(st, hub, service.Options{
		SessionStale:   cfg.SessionStale,
		EagerMaxBytes:  cfg.EagerMaxBytes,
		EagerMaxFiles:  cfg.EagerMaxFiles,
		StorageQuota:   cfg.Limits.SessionStorageBytes,
		PublicURL:      cfg.PublicURL,
		Retention:      cfg.Retention,
		AbandonAfter:   cfg.AbandonAfter,
		Protected:      cfg.ProtectedSessions,
		BlobCacheBytes: cfg.BlobCacheBytes,
	}, log)

	done := make(chan struct{})
	api := httpapi.New(httpapi.Options{
		Service: svc,
		Events:  hub,
		Config:  cfg,
		Limiter: limiter,
		Log:     log,
		Done:    done,
		Ready: func(ctx context.Context) error {
			if err := st.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			if rdb != nil {
				if err := rdb.Ping(ctx).Err(); err != nil {
					return fmt.Errorf("redis: %w", err)
				}
			}
			return nil
		},
	})

	bgCtx, stopBackground := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Go(func() { hub.Run(bgCtx) })
	bg.Go(func() { svc.RunCleanup(bgCtx, cfg.CleanupInterval) })

	var tlsConfig *tls.Config
	if cfg.TLS() {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	servers := make([]*http.Server, 0, len(cfg.Listen))
	errs := make(chan error, len(cfg.Listen))
	for _, addr := range cfg.Listen {
		ln, err := listen(ctx, addr)
		if err != nil {
			stopBackground()
			return err
		}
		srv := &http.Server{
			Handler:           api,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			TLSConfig:         tlsConfig,
			ErrorLog:          errLog,
		}
		servers = append(servers, srv)
		go func() {
			var err error
			if cfg.TLS() {
				err = srv.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
			} else {
				err = srv.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("serve %s: %w", addr, err)
			}
		}()
		log.Info("listening", "address", ln.Addr().String(), "tls", cfg.TLS())
	}
	log.Info("ready",
		"access_keys_required", len(cfg.AccessKeys) > 0,
		"redis", rdb != nil,
		"trusted_proxies", len(cfg.TrustedProxies),
		"cors_origins", len(cfg.CORSOrigins),
	)

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case serveErr = <-errs:
		log.Error("server failed", "error", serveErr)
	}
	close(done) // ends live streams so Shutdown can finish
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("forced shutdown", "error", err)
		}
	}
	stopBackground()
	bg.Wait()
	return serveErr
}

// listen opens one BIGA_LISTEN address: "host:port", "[ipv6]:port",
// ":port" (all interfaces, IPv4 and IPv6), "tcp4:" or "tcp6:" to pin a
// family, or "unix:/path/to.sock".
func listen(ctx context.Context, addr string) (net.Listener, error) {
	network, address := "tcp", addr
	for _, prefix := range []string{"tcp4", "tcp6", "unix"} {
		if rest, ok := strings.CutPrefix(addr, prefix+":"); ok {
			network, address = prefix, rest
			break
		}
	}
	if network == "unix" {
		_ = os.Remove(address) // a stale socket from a previous run
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "unix", address)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		// Group-writable so a reverse proxy in the same group can connect.
		_ = os.Chmod(address, 0o660) //nolint:gosec // G302: see above
		return ln, nil
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

func cleanup(ctx context.Context, cfg config.Config, log *slog.Logger, stdout io.Writer) error {
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	st, err := store.Open(openCtx, cfg.DatabaseURL, log)
	cancel()
	if err != nil {
		return err
	}
	defer st.Close()
	svc := service.New(st, events.NewHub(nil, log), service.Options{
		Retention: cfg.Retention, AbandonAfter: cfg.AbandonAfter, Protected: cfg.ProtectedSessions,
	}, log)
	res, err := svc.Cleanup(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(res)
}

// healthcheck probes the local server. It reads only BIGA_LISTEN and the
// TLS settings, so it works in a container without the database secrets.
func healthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	target := fs.String("url", "", "URL to probe (default: derived from BIGA_LISTEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	url := *target
	if url == "" {
		addr := strings.TrimSpace(strings.Split(os.Getenv("BIGA_LISTEN")+",", ",")[0])
		if addr == "" {
			addr = ":8080"
		}
		scheme := "http"
		if os.Getenv("BIGA_TLS_CERT_FILE") != "" {
			scheme = "https"
			// Probing ourselves over loopback; the certificate names the
			// public host, not 127.0.0.1.
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // loopback self-probe
		}
		if sock, ok := strings.CutPrefix(addr, "unix:"); ok {
			client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}}
			url = "http://unix/healthz"
		} else {
			addr = strings.TrimPrefix(strings.TrimPrefix(addr, "tcp4:"), "tcp6:")
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("BIGA_LISTEN: %w", err)
			}
			switch host {
			case "", "0.0.0.0":
				host = "127.0.0.1"
			case "::":
				host = "::1"
			}
			url = scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
		}
	}
	// The URL comes from the operator's own flag or BIGA_LISTEN.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: operator-chosen target
	if err != nil {
		return err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: operator-chosen target
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
