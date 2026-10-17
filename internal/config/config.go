// Package config resolves the server's settings from the environment.
//
// Every setting has a BIGA_ name, apart from the conventional DATABASE_URL
// and REDIS_URL. Secrets can be supplied through a file instead (NAME_FILE),
// which is how Docker and Kubernetes secrets are mounted.
package config

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved server configuration.
type Config struct {
	// DatabaseURL is a Postgres connection string.
	DatabaseURL string
	// RedisURL enables cross-instance live events and shared rate limits.
	// Empty runs single-instance.
	RedisURL string

	// Listen holds the addresses to serve on: "host:port", "[ipv6]:port",
	// ":port" (every interface, IPv4 and IPv6) or "unix:/path".
	Listen []string
	// TLSCertFile and TLSKeyFile make the server terminate TLS itself. Both
	// or neither must be set.
	TLSCertFile string
	TLSKeyFile  string

	// PublicURL is the origin viewers open, used to build share links. Empty
	// leaves link building to the CLI.
	PublicURL string
	// CORSOrigins may read the API from a browser on another origin. "*"
	// allows any. Writes never get CORS headers.
	CORSOrigins []string
	// TrustedProxies are the peers whose X-Forwarded-For is believed. Every
	// other peer is the client itself.
	TrustedProxies []netip.Prefix

	// AccessKeys, when set, are required (X-Biga-Key) for every write, so
	// only people holding a key can broadcast through this server.
	AccessKeys []string
	// CronSecret enables GET /api/cron/cleanup for external schedulers.
	CronSecret string

	// MigrateOnStart applies database migrations before serving.
	MigrateOnStart bool

	// Retention is how long an ended session is kept before deletion.
	Retention time.Duration
	// AbandonAfter is how long an active session may go silent before the
	// cleanup marks it ended.
	AbandonAfter time.Duration
	// CleanupInterval is how often the retention sweep runs. Zero disables it.
	CleanupInterval time.Duration
	// ProtectedSessions are never deleted by the sweep.
	ProtectedSessions []string

	// SessionStale is how long a broadcast may go without a heartbeat before
	// viewers see it as ended.
	SessionStale time.Duration
	// EagerMaxBytes and EagerMaxFiles bound the sessions whose file contents
	// ship with the initial state.
	EagerMaxBytes int64
	EagerMaxFiles int

	// BlobCacheBytes bounds the in-process cache of file contents.
	BlobCacheBytes int64

	Limits Limits

	LogLevel  slog.Level
	LogFormat string
}

// Limits bounds what one client can make the server do.
type Limits struct {
	// SnapshotBytes, CheckpointBytes and SessionBytes cap request bodies,
	// measured after decompression.
	SnapshotBytes   int64
	CheckpointBytes int64
	SessionBytes    int64
	// FileBytes caps one file's content.
	FileBytes int64
	// SnapshotFiles caps the files in one snapshot, CheckpointEntries the
	// entries in one checkpoint.
	SnapshotFiles     int
	CheckpointEntries int
	// SessionStorageBytes caps the stored content of one session.
	SessionStorageBytes int64
	// ReadsPerMinute is the per-client budget for viewer requests. Zero
	// disables the limit.
	ReadsPerMinute int
	// Streams caps open live streams overall, StreamsPerClient per client.
	Streams          int
	StreamsPerClient int
}

// TLS reports whether the server terminates TLS itself.
func (c Config) TLS() bool { return c.TLSCertFile != "" }

// Load reads the configuration using getenv, normally [os.Getenv].
func Load(getenv func(string) string) (Config, error) {
	e := env{getenv: getenv}
	c := Config{
		DatabaseURL:    e.secret("DATABASE_URL", "BIGA_DATABASE_URL"),
		RedisURL:       e.secret("REDIS_URL", "BIGA_REDIS_URL"),
		Listen:         list(cmp.Or(e.get("BIGA_LISTEN"), ":8080")),
		TLSCertFile:    e.get("BIGA_TLS_CERT_FILE"),
		TLSKeyFile:     e.get("BIGA_TLS_KEY_FILE"),
		CronSecret:     e.secret("BIGA_CRON_SECRET"),
		MigrateOnStart: e.boolean("BIGA_MIGRATE_ON_START", true),
		LogFormat:      cmp.Or(strings.ToLower(e.get("BIGA_LOG_FORMAT")), "json"),
		Limits: Limits{
			SnapshotBytes:       e.int64("MAX_SNAPSHOT_BYTES", 32<<20),
			CheckpointBytes:     e.int64("MAX_CHECKPOINT_BYTES", 8<<20),
			SessionBytes:        e.int64("MAX_SESSION_BYTES", 16<<10),
			FileBytes:           e.int64("MAX_FILE_BYTES", 6<<20),
			SnapshotFiles:       e.int("MAX_SNAPSHOT_FILES", 50_000),
			CheckpointEntries:   e.int("MAX_CHECKPOINT_ENTRIES", 10_000),
			SessionStorageBytes: e.int64("MAX_SESSION_STORAGE_BYTES", 1<<30),
			ReadsPerMinute:      e.intOrZero("READ_RATE_LIMIT", 3000),
			Streams:             e.int("MAX_STREAMS", 10_000),
			StreamsPerClient:    e.int("MAX_STREAMS_PER_CLIENT", 32),
		},
		Retention:       time.Duration(e.int("RETENTION_DAYS", 30)) * 24 * time.Hour,
		AbandonAfter:    e.duration("BIGA_ABANDON_AFTER", 24*time.Hour),
		CleanupInterval: e.duration("BIGA_CLEANUP_INTERVAL", time.Hour),
		SessionStale:    time.Duration(e.int("SESSION_STALE_MS", 45_000)) * time.Millisecond,
		EagerMaxBytes:   e.int64("EAGER_MAX_BYTES", 1536<<10),
		EagerMaxFiles:   e.int("EAGER_MAX_FILES", 400),
		BlobCacheBytes:  e.int64("BLOB_CACHE_BYTES", 64<<20),
	}

	c.ProtectedSessions = list(e.get("BIGA_PROTECTED_SESSIONS"))
	if demo := e.get("BIGA_DEMO_SESSION_ID"); demo != "" {
		c.ProtectedSessions = append(c.ProtectedSessions, demo)
	}
	c.LogLevel = e.level("BIGA_LOG_LEVEL")

	if raw := e.get("BIGA_PUBLIC_URL"); raw != "" {
		u, err := origin(raw, true)
		if err != nil {
			e.fail("BIGA_PUBLIC_URL", err)
		}
		c.PublicURL = u
	}
	for _, o := range list(e.get("BIGA_CORS_ORIGINS")) {
		if o == "*" {
			c.CORSOrigins = append(c.CORSOrigins, o)
			continue
		}
		u, err := origin(o, false)
		if err != nil {
			e.fail("BIGA_CORS_ORIGINS", err)
			continue
		}
		c.CORSOrigins = append(c.CORSOrigins, u)
	}
	c.TrustedProxies = e.prefixes("BIGA_TRUSTED_PROXIES")
	c.AccessKeys = e.accessKeys()

	switch {
	case c.DatabaseURL == "":
		e.fail("DATABASE_URL", errors.New("is required"))
	case (c.TLSCertFile == "") != (c.TLSKeyFile == ""):
		e.fail("BIGA_TLS_CERT_FILE", errors.New("and BIGA_TLS_KEY_FILE must be set together"))
	case c.LogFormat != "json" && c.LogFormat != "text":
		e.fail("BIGA_LOG_FORMAT", fmt.Errorf("%q is not json or text", c.LogFormat))
	}
	if len(c.Listen) == 0 {
		e.fail("BIGA_LISTEN", errors.New("has no addresses"))
	}
	if c.CronSecret != "" && len(c.CronSecret) < 16 {
		e.fail("BIGA_CRON_SECRET", errors.New("must be at least 16 characters"))
	}
	return c, errors.Join(e.errs...)
}

// env reads settings and collects every problem, so a misconfigured
// deployment learns about all of them at once.
type env struct {
	getenv func(string) string
	errs   []error
}

func (e *env) fail(name string, err error) {
	e.errs = append(e.errs, fmt.Errorf("%s %w", name, err))
}

func (e *env) get(name string) string { return strings.TrimSpace(e.getenv(name)) }

// secret reads the first of names that is set, either directly or through
// a NAME_FILE pointing at a file holding the value.
func (e *env) secret(names ...string) string {
	for _, name := range names {
		if v := e.get(name); v != "" {
			return v
		}
		if path := e.get(name + "_FILE"); path != "" {
			data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the secret file
			if err != nil {
				e.fail(name+"_FILE", err)
				return ""
			}
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func (e *env) int64(name string, fallback int64) int64 {
	raw := e.get("BIGA_" + name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		e.fail("BIGA_"+name, fmt.Errorf("%q is not a positive integer", raw))
		return fallback
	}
	return n
}

func (e *env) int(name string, fallback int) int {
	return int(e.int64(name, int64(fallback)))
}

// intOrZero is int, but "0" is allowed and means "off".
func (e *env) intOrZero(name string, fallback int) int {
	if e.get("BIGA_"+name) == "0" {
		return 0
	}
	return e.int(name, fallback)
}

func (e *env) duration(name string, fallback time.Duration) time.Duration {
	raw := e.get(name)
	if raw == "" {
		return fallback
	}
	if raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		e.fail(name, fmt.Errorf("%q is not a duration like 30m or 1h", raw))
		return fallback
	}
	return d
}

func (e *env) boolean(name string, fallback bool) bool {
	raw := e.get(name)
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		e.fail(name, fmt.Errorf("%q is not true or false", raw))
		return fallback
	}
	return b
}

func (e *env) level(name string) slog.Level {
	var l slog.Level
	raw := e.get(name)
	if raw == "" {
		return slog.LevelInfo
	}
	if err := l.UnmarshalText([]byte(raw)); err != nil {
		e.fail(name, fmt.Errorf("%q is not debug, info, warn or error", raw))
		return slog.LevelInfo
	}
	return l
}

// Named sets of proxy addresses accepted in BIGA_TRUSTED_PROXIES.
var proxyKeywords = map[string][]string{
	"loopback": {"127.0.0.0/8", "::1/128"},
	"private": {
		"127.0.0.0/8", "::1/128", // loopback
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC 1918
		"100.64.0.0/10", // carrier-grade NAT, also Tailscale
		"fc00::/7",      // IPv6 unique local
	},
}

func (e *env) prefixes(name string) []netip.Prefix {
	var out []netip.Prefix
	for _, item := range list(e.get(name)) {
		if set, ok := proxyKeywords[strings.ToLower(item)]; ok {
			for _, p := range set {
				out = append(out, netip.MustParsePrefix(p))
			}
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			e.fail(name, fmt.Errorf("%q is not an IP address, CIDR, loopback or private", item))
			continue
		}
		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return out
}

// MinAccessKeyLen is the shortest access key accepted; anything shorter is
// guessable.
const MinAccessKeyLen = 24

func (e *env) accessKeys() []string {
	keys := list(e.get("BIGA_ACCESS_KEYS"))
	if path := e.get("BIGA_ACCESS_KEYS_FILE"); path != "" {
		fromFile, err := readKeyFile(path)
		if err != nil {
			e.fail("BIGA_ACCESS_KEYS_FILE", err)
		}
		keys = append(keys, fromFile...)
	}
	for _, k := range keys {
		if len(k) < MinAccessKeyLen {
			e.fail("BIGA_ACCESS_KEYS", fmt.Errorf("has a key shorter than %d characters; generate one with `biga-server gen-key`", MinAccessKeyLen))
			break
		}
	}
	return keys
}

// readKeyFile reads one key per line, skipping blank lines and # comments.
func readKeyFile(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the key file
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var keys []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	return keys, s.Err()
}

// origin validates an http(s) URL and returns it without a trailing slash.
// A browser origin has no path, so allowPath is false for CORS origins.
func origin(raw string, allowPath bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q must be an http or https URL", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q must not carry credentials, a query or a fragment", raw)
	}
	if !allowPath && strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("%q must be an origin, without a path", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// list splits a comma- or whitespace-separated setting.
func list(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
}
