package httpapi

// These tests drive the real API against Postgres. Point
// BIGA_TEST_DATABASE_URL at a scratch database to run them, e.g.
//
//	docker run -d -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD=test postgres:17-alpine
//	BIGA_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/postgres go test ./...

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/biga-sh/server/internal/config"
	"github.com/biga-sh/server/internal/events"
	"github.com/biga-sh/server/internal/ratelimit"
	"github.com/biga-sh/server/internal/service"
	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/wire"
)

var (
	migrateOnce sync.Once
	migrateErr  error
	quiet       = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	dsn := os.Getenv("BIGA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BIGA_TEST_DATABASE_URL is not set")
	}
	migrateOnce.Do(func() { migrateErr = store.Migrate(context.Background(), dsn, quiet) })
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	vars := map[string]string{"DATABASE_URL": dsn, "BIGA_PUBLIC_URL": "https://biga.example"}
	maps.Copy(vars, env)
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	st, err := store.Open(t.Context(), dsn, quiet)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	hub := events.NewHub(nil, quiet)
	svc := service.New(st, hub, service.Options{
		SessionStale:   cfg.SessionStale,
		EagerMaxBytes:  cfg.EagerMaxBytes,
		EagerMaxFiles:  cfg.EagerMaxFiles,
		StorageQuota:   cfg.Limits.SessionStorageBytes,
		PublicURL:      cfg.PublicURL,
		BlobCacheBytes: cfg.BlobCacheBytes,
	}, quiet)
	done := make(chan struct{})
	api := New(Options{Service: svc, Events: hub, Config: cfg, Limiter: ratelimit.NewMemory(), Log: quiet, Done: done, Ready: st.Ping})
	srv := httptest.NewServer(api)
	t.Cleanup(func() { close(done); srv.Close() })
	return &harness{t: t, srv: srv, st: st}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) data(t *testing.T, v any) {
	t.Helper()
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.body, &env); err != nil || !env.OK {
		t.Fatalf("not a success envelope (%d): %s", r.status, r.body)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decode data: %v", err)
	}
}

func (r response) code() string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &env)
	return env.Error.Code
}

type req struct {
	method, path string
	body         any
	raw          []byte
	token, key   string
	header       map[string]string
}

func (h *harness) do(q req) response {
	h.t.Helper()
	var body io.Reader
	switch {
	case q.raw != nil:
		body = bytes.NewReader(q.raw)
	case q.body != nil:
		b, err := json.Marshal(q.body)
		if err != nil {
			h.t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(h.t.Context(), q.method, h.srv.URL+q.path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if q.token != "" {
		r.Header.Set("Authorization", "Bearer "+q.token)
	}
	if q.key != "" {
		r.Header.Set("X-Biga-Key", q.key)
	}
	for k, v := range q.header {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func (h *harness) expect(q req, status int, code string) response {
	h.t.Helper()
	r := h.do(q)
	if r.status != status || (code != "" && r.code() != code) {
		h.t.Fatalf("%s %s: got %d %q, want %d %q: %s", q.method, q.path, r.status, r.code(), status, code, r.body)
	}
	return r
}

func newSessionID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "t" + hex.EncodeToString(b)
}

// create makes a session and returns its owner token.
func (h *harness) create(id string) string {
	h.t.Helper()
	var grant wire.SessionGrant
	h.expect(req{method: "PUT", path: "/api/sessions/" + id, body: map[string]string{"title": "demo"}}, 201, "").data(h.t, &grant)
	if !strings.HasPrefix(grant.Token, "bgs_") {
		h.t.Fatalf("create returned no owner token: %+v", grant)
	}
	return grant.Token
}

func file(name, content string) map[string]any {
	return map[string]any{"type": "file", "name": name, "path": name, "size": len(content), "hash": wire.HashOf(content), "content": content}
}

func snapshot(id string, files ...map[string]any) map[string]any {
	children := make([]any, len(files))
	for i, f := range files {
		children[i] = f
	}
	return map[string]any{
		"sessionId": id, "timestamp": time.Now().UnixMilli(), "sequenceNum": 0,
		"tree": map[string]any{"type": "directory", "name": "proj", "path": ".", "children": children},
	}
}

func entry(path, change, content string) map[string]any {
	e := map[string]any{"path": path, "changeType": change, "size": len(content)}
	if change != "deleted" {
		e["hash"], e["content"] = wire.HashOf(content), content
	} else {
		e["hash"] = nil
	}
	return e
}

func checkpoint(id string, entries ...map[string]any) map[string]any {
	return map[string]any{"sessionId": id, "timestamp": time.Now().UnixMilli(), "entries": entries}
}

func TestOwnerTokenGuardsEveryWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	base := "/api/sessions/" + id

	var grant wire.SessionGrant
	r := h.expect(req{method: "PUT", path: base, body: map[string]string{"title": "demo"}}, 201, "")
	r.data(t, &grant)
	token := grant.Token
	if grant.ShareURL != "https://biga.example/session/"+id {
		t.Errorf("shareUrl = %q", grant.ShareURL)
	}
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Encoding") != "" {
		t.Errorf("a response carrying a token must be uncached and uncompressed: %v", r.header)
	}
	other := h.create(newSessionID(t)) // a valid token for a different session

	// Without the owner token, nobody can take the session over.
	h.expect(req{method: "PUT", path: base, body: map[string]string{"title": "pwned"}}, 401, "unauthorized")
	h.expect(req{method: "PUT", path: base, token: other, body: map[string]string{"title": "pwned"}}, 403, "forbidden")
	h.expect(req{method: "POST", path: base + "/snapshot", body: snapshot(id, file("a.txt", "evil\n"))}, 401, "unauthorized")
	h.expect(req{method: "POST", path: base + "/snapshot", token: other, body: snapshot(id, file("a.txt", "evil\n"))}, 403, "forbidden")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: "bgs_guess", body: checkpoint(id, entry("a.txt", "added", "x"))}, 403, "forbidden")
	h.expect(req{method: "POST", path: base + "/heartbeat"}, 401, "unauthorized")
	h.expect(req{method: "GET", path: base + "/manifest"}, 401, "unauthorized")
	h.expect(req{method: "DELETE", path: base}, 401, "unauthorized")

	// The owner can do everything.
	var again wire.SessionGrant
	h.expect(req{method: "PUT", path: base, token: token, body: map[string]string{}}, 200, "").data(t, &again)
	if again.Token != "" || again.Title != "demo" {
		t.Errorf("reactivation leaked a token or lost the title: %+v", again)
	}
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("a.txt", "one\ntwo\n"), file("b.txt", "keep\n"))}, 202, "")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, body: checkpoint(id, entry("a.txt", "modified", "one\n2\nthree\n"))}, 202, "")
	h.expect(req{method: "POST", path: base + "/heartbeat", token: token}, 204, "")

	var m wire.Manifest
	h.expect(req{method: "GET", path: base + "/manifest", token: token}, 200, "").data(t, &m)
	if len(m.Files) != 2 {
		t.Errorf("manifest = %+v", m)
	}

	// Viewers read without credentials.
	var st wire.SessionState
	h.expect(req{method: "GET", path: base + "/state"}, 200, "").data(t, &st)
	if st.Mode != wire.ModeEager || len(st.Checkpoints) != 2 || st.Snapshot == nil {
		t.Fatalf("state = %+v", st)
	}
	latest := st.Checkpoints[0]
	if latest.Seq != 1 || latest.Label != "Updated a.txt" || latest.Additions != 2 || latest.Deletions != 1 {
		t.Errorf("latest checkpoint = %+v", latest)
	}
	if st.Checkpoints[1].Label != "Initial snapshot" || st.Checkpoints[1].Additions != 3 {
		t.Errorf("initial checkpoint = %+v", st.Checkpoints[1])
	}

	var fc wire.FileContent
	h.expect(req{method: "GET", path: base + "/file?path=a.txt"}, 200, "").data(t, &fc)
	if fc.Content != "one\n2\nthree\n" {
		t.Errorf("latest content = %q", fc.Content)
	}
	h.expect(req{method: "GET", path: base + "/file?path=a.txt&ref=0"}, 200, "").data(t, &fc)
	if fc.Content != "one\ntwo\n" {
		t.Errorf("content at seq 0 = %q", fc.Content)
	}
	r = h.expect(req{method: "GET", path: base + "/file?path=a.txt&ref=" + wire.HashOf("one\ntwo\n")}, 200, "")
	if !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Errorf("hash refs should be immutable: %q", r.header.Get("Cache-Control"))
	}

	var cp wire.Checkpoint
	h.expect(req{method: "GET", path: base + "/checkpoints/1"}, 200, "").data(t, &cp)
	if len(cp.Changes) != 1 || cp.Changes[0].BeforeHash == nil || *cp.Changes[0].BeforeHash != wire.HashOf("one\ntwo\n") {
		t.Errorf("checkpoint 1 = %+v", cp)
	}

	// Ending is the owner's call, and only once.
	h.expect(req{method: "DELETE", path: base, token: token}, 204, "")
	h.expect(req{method: "DELETE", path: base, token: token}, 409, "invalid_state")
	h.expect(req{method: "POST", path: base + "/heartbeat", token: token}, 409, "inactive_session")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, body: checkpoint(id, entry("a.txt", "deleted", ""))}, 409, "invalid_state")
}

func TestRejoinSnapshotRecordsDeletions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("a.txt", "a\n"), file("gone.txt", "1\n2\n"))}, 202, "")
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("a.txt", "a\n"))}, 202, "")

	var list struct {
		Checkpoints []wire.CheckpointSummary `json:"checkpoints"`
	}
	h.expect(req{method: "GET", path: base + "/checkpoints?limit=10"}, 200, "").data(t, &list)
	if len(list.Checkpoints) != 2 || list.Checkpoints[0].Label != "Resumed session" || list.Checkpoints[0].Deletions != 2 {
		t.Fatalf("checkpoints = %+v", list.Checkpoints)
	}
	h.expect(req{method: "GET", path: base + "/file?path=gone.txt"}, 404, "not_found")
	var history struct {
		History []wire.FileHistoryEntry `json:"history"`
	}
	h.expect(req{method: "GET", path: base + "/file/history?path=gone.txt"}, 200, "").data(t, &history)
	if len(history.History) != 2 || history.History[0].ChangeType != wire.Deleted {
		t.Errorf("history = %+v", history.History)
	}
}

func TestSessionsWithoutATokenAreReadOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	// A row with no owner token, as created before tokens existed.
	if _, _, err := h.st.Q().InsertSession(t.Context(), id, "legacy", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	base := "/api/sessions/" + id
	h.expect(req{method: "GET", path: base}, 200, "")
	h.expect(req{method: "PUT", path: base, body: map[string]string{}}, 403, "session_locked")
	h.expect(req{method: "PUT", path: base, token: "bgs_anything", body: map[string]string{}}, 403, "session_locked")
	h.expect(req{method: "POST", path: base + "/snapshot", token: "bgs_anything", body: snapshot(id, file("a", "x"))}, 403, "session_locked")
	h.expect(req{method: "DELETE", path: base}, 403, "session_locked")
}

func TestRejectsMalformedUploads(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]string{"BIGA_MAX_CHECKPOINT_BYTES": "4096"})
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id

	forged := file("a.txt", "real\n")
	forged["content"] = "something else\n"
	traversal := entry("../../etc/passwd", "added", "x")
	nul := entry("a\x00b", "added", "x")
	nested := map[string]any{"type": "file", "name": "x", "path": "elsewhere/x", "size": 1, "hash": wire.HashOf("y"), "content": "y"}

	for name, q := range map[string]req{
		"hash that does not match content": {method: "POST", path: base + "/snapshot", body: snapshot(id, forged)},
		"path that lies about its place":   {method: "POST", path: base + "/snapshot", body: snapshot(id, nested)},
		"duplicate paths":                  {method: "POST", path: base + "/snapshot", body: snapshot(id, file("a", "1"), file("a", "1"))},
		"sessionId of another session":     {method: "POST", path: base + "/snapshot", body: snapshot("someoneelse", file("a", "1"))},
		"path traversal":                   {method: "POST", path: base + "/checkpoint", body: checkpoint(id, traversal)},
		"NUL byte in a path":               {method: "POST", path: base + "/checkpoint", body: checkpoint(id, nul)},
		"unknown change type":              {method: "POST", path: base + "/checkpoint", body: checkpoint(id, map[string]any{"path": "a", "changeType": "exploded", "size": 0})},
		"empty checkpoint":                 {method: "POST", path: base + "/checkpoint", body: checkpoint(id)},
		"not JSON":                         {method: "POST", path: base + "/checkpoint", raw: []byte("{nope")},
		"title with control characters":    {method: "PUT", path: base, body: map[string]string{"title": "a\x1b[31mb"}},
	} {
		q.token = token
		r := h.do(q)
		if r.status != http.StatusBadRequest || r.code() != "invalid_request" {
			t.Errorf("%s: got %d %q: %s", name, r.status, r.code(), r.body)
		}
	}

	h.expect(req{method: "GET", path: "/api/sessions/bad..id/state"}, 400, "invalid_request")
	h.expect(req{method: "GET", path: base + "/file?path=../secret"}, 400, "invalid_request")
	h.expect(req{method: "GET", path: base + "/checkpoints?limit=-1"}, 400, "invalid_request")

	// Size limits apply to the decoded body, so a gzip bomb is refused.
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write([]byte(`{"sessionId":"` + id + `","timestamp":1,"entries":[{"path":"a","changeType":"added","size":1,"content":"`))
	_, _ = zw.Write(bytes.Repeat([]byte("A"), 1<<20))
	_ = zw.Close()
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, raw: bomb.Bytes(), header: map[string]string{"Content-Encoding": "gzip"}}, 413, "payload_too_large")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, raw: []byte("{}"), header: map[string]string{"Content-Encoding": "br"}}, 415, "unsupported_encoding")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, raw: []byte("a=b"), header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, 415, "unsupported_media_type")

	// Nothing above was stored.
	var st wire.SessionState
	h.expect(req{method: "GET", path: base + "/state"}, 200, "").data(t, &st)
	if st.Snapshot != nil || len(st.Checkpoints) != 0 {
		t.Errorf("rejected uploads left data behind: %+v", st)
	}
}

func TestAccessKeyGatesWrites(t *testing.T) {
	t.Parallel()
	key := "bgk_" + strings.Repeat("k", 40)
	h := newHarness(t, map[string]string{"BIGA_ACCESS_KEYS": key})
	id := newSessionID(t)
	base := "/api/sessions/" + id

	h.expect(req{method: "PUT", path: base, body: map[string]string{}}, 401, "access_key_required")
	h.expect(req{method: "PUT", path: base, key: key + "x", body: map[string]string{}}, 401, "access_key_invalid")
	var grant wire.SessionGrant
	h.expect(req{method: "PUT", path: base, key: key, body: map[string]string{}}, 201, "").data(t, &grant)
	// Even the owner needs the key, so revoking a key stops its broadcasts.
	h.expect(req{method: "POST", path: base + "/heartbeat", token: grant.Token}, 401, "access_key_required")
	h.expect(req{method: "POST", path: base + "/heartbeat", token: grant.Token, key: key}, 204, "")
	// Viewers never need one.
	h.expect(req{method: "GET", path: base + "/state"}, 200, "")
}

func TestRepeatedBadCredentialsAreLockedOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	for range limitAuthFailures.n {
		h.expect(req{method: "POST", path: base + "/heartbeat", token: "bgs_wrong"}, 403, "forbidden")
	}
	r := h.expect(req{method: "POST", path: base + "/heartbeat", token: "bgs_wrong"}, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("lockout should say when to retry")
	}
	// The lockout is per client: it blocks even a correct guess from it.
	h.expect(req{method: "POST", path: base + "/heartbeat", token: token}, 429, "rate_limited")
}

func TestStorageQuota(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]string{"BIGA_MAX_SESSION_STORAGE_BYTES": "1000"})
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	big := make([]byte, 3000)
	_, _ = rand.Read(big) // incompressible
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("big.txt", hex.EncodeToString(big)))}, 413, "storage_quota_exceeded")
	var st wire.SessionState
	h.expect(req{method: "GET", path: base + "/state"}, 200, "").data(t, &st)
	if st.Snapshot != nil {
		t.Error("a snapshot over quota was stored")
	}
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("small.txt", "ok\n"))}, 202, "")
}

func TestCORSIsReadOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]string{"BIGA_CORS_ORIGINS": "https://viewer.example"})
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	origin := map[string]string{"Origin": "https://viewer.example"}

	r := h.expect(req{method: "GET", path: base, header: origin}, 200, "")
	if r.header.Get("Access-Control-Allow-Origin") != "https://viewer.example" {
		t.Errorf("read ACAO = %q", r.header.Get("Access-Control-Allow-Origin"))
	}
	r = h.expect(req{method: "GET", path: base, header: map[string]string{"Origin": "https://evil.example"}}, 200, "")
	if r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("an unlisted origin got CORS access")
	}
	r = h.expect(req{method: "POST", path: base + "/heartbeat", token: token, header: origin}, 204, "")
	if r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("a write route sent CORS headers")
	}
	for _, header := range []string{"X-Content-Type-Options", "Content-Security-Policy", "X-Request-Id"} {
		if r.header.Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
}

func TestStreamDeliversLiveEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("a.txt", "1\n"))}, 202, "")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+base+"/stream", nil)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events := make(chan string, 16)
	go func() {
		s := bufio.NewScanner(resp.Body)
		for s.Scan() {
			if name, ok := strings.CutPrefix(s.Text(), "event: "); ok {
				events <- name
			}
		}
		close(events)
	}()
	next := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	next("connected")
	h.expect(req{method: "POST", path: base + "/checkpoint", token: token, body: checkpoint(id, entry("a.txt", "modified", "2\n"))}, 202, "")
	next("checkpoint")
	h.expect(req{method: "DELETE", path: base, token: token}, 204, "")
	next("session_ended")
	if _, open := <-events; open {
		t.Error("the stream should close after session_ended")
	}
}

func TestUnknownRoutesAnswerJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.expect(req{method: "GET", path: "/api/nope"}, 404, "not_found")
	h.expect(req{method: "GET", path: "/readyz"}, 200, "")
	h.expect(req{method: "GET", path: "/api/cron/cleanup"}, 404, "not_found") // disabled without a secret
}

func TestResponsesAreCompressedForGzipClients(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newSessionID(t)
	token := h.create(id)
	base := "/api/sessions/" + id
	h.expect(req{method: "POST", path: base + "/snapshot", token: token, body: snapshot(id, file("a.txt", strings.Repeat("hello world\n", 500)))}, 202, "")

	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+base+"/state", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("headers = %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.NewDecoder(zr).Decode(&env); err != nil || env["ok"] != true {
		t.Fatalf("decoded = %v, %v", env, err)
	}
}
