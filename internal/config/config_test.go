package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	return Load(func(k string) string { return env[k] })
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	c, err := load(t, map[string]string{"DATABASE_URL": "postgres://x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Listen) != 1 || c.Listen[0] != ":8080" {
		t.Errorf("Listen = %v", c.Listen)
	}
	if c.Retention != 30*24*time.Hour || c.SessionStale != 45*time.Second || c.CleanupInterval != time.Hour {
		t.Errorf("timings = %v %v %v", c.Retention, c.SessionStale, c.CleanupInterval)
	}
	if c.Limits.SnapshotBytes != 32<<20 || c.Limits.ReadsPerMinute != 3000 || !c.MigrateOnStart {
		t.Errorf("limits = %+v", c.Limits)
	}
	if len(c.AccessKeys) != 0 || len(c.TrustedProxies) != 0 || c.TLS() {
		t.Error("security features should be opt-in")
	}
}

func TestSecretsFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "dsn")
	keys := filepath.Join(dir, "keys")
	if err := os.WriteFile(dsn, []byte("postgres://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", MinAccessKeyLen)
	if err := os.WriteFile(keys, []byte("# team\n"+key+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := load(t, map[string]string{
		"DATABASE_URL_FILE":     dsn,
		"BIGA_ACCESS_KEYS_FILE": keys,
		"BIGA_RETENTION_DAYS":   "7",
		"BIGA_DEMO_SESSION_ID":  "demo1",
		"BIGA_READ_RATE_LIMIT":  "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://from-file" || len(c.AccessKeys) != 1 || c.AccessKeys[0] != key {
		t.Errorf("secrets = %q %v", c.DatabaseURL, c.AccessKeys)
	}
	if c.Retention != 7*24*time.Hour || len(c.ProtectedSessions) != 1 || c.Limits.ReadsPerMinute != 0 {
		t.Errorf("settings = %v %v %d", c.Retention, c.ProtectedSessions, c.Limits.ReadsPerMinute)
	}
}

func TestTrustedProxies(t *testing.T) {
	t.Parallel()
	c, err := load(t, map[string]string{"DATABASE_URL": "x", "BIGA_TRUSTED_PROXIES": "loopback, 10.1.2.3, fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	contains := func(addr string) bool {
		for _, p := range c.TrustedProxies {
			if p.Contains(netip.MustParseAddr(addr)) {
				return true
			}
		}
		return false
	}
	for addr, want := range map[string]bool{"127.0.0.1": true, "::1": true, "10.1.2.3": true, "10.1.2.4": false, "fd12::1": true, "8.8.8.8": false} {
		if contains(addr) != want {
			t.Errorf("trusted(%s) = %v, want %v", addr, !want, want)
		}
	}
}

func TestReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := load(t, map[string]string{
		"BIGA_ACCESS_KEYS":      "short",
		"BIGA_PUBLIC_URL":       "ftp://x",
		"BIGA_CORS_ORIGINS":     "https://ok.example,https://x.example/path",
		"BIGA_TRUSTED_PROXIES":  "not-an-ip",
		"BIGA_RETENTION_DAYS":   "-1",
		"BIGA_TLS_CERT_FILE":    "cert.pem",
		"BIGA_CLEANUP_INTERVAL": "soon",
	})
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for _, want := range []string{"DATABASE_URL", "BIGA_ACCESS_KEYS", "BIGA_PUBLIC_URL", "BIGA_CORS_ORIGINS", "BIGA_TRUSTED_PROXIES", "BIGA_RETENTION_DAYS", "BIGA_CLEANUP_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}
