package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"time"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	clientAddrKey
)

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func clientAddr(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(clientAddrKey).(netip.Addr)
	return a
}

// recorder captures the status and size of a response for the access log.
// Unwrap keeps http.ResponseController (deadlines, flushing) working.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// base wraps every request: an ID, the client address, security headers,
// panic recovery and an access log line. Credentials and query strings are
// never logged.
func (a *API) base(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		addr := a.clients.addr(r)
		ctx := context.WithValue(context.WithValue(r.Context(), requestIDKey, id), clientAddrKey, addr)
		r = r.WithContext(ctx)

		h := w.Header()
		h.Set("X-Request-Id", id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", a.corp)
		if a.tls {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		rec := &recorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				a.log.Error("panic serving request", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if rec.status == 0 {
					fail(rec, r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred.", nil)
				}
			}
			level := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				level = slog.LevelDebug
			}
			a.log.Log(ctx, level, "request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"client", clientKey(addr),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// deadlines bounds how long reading the request and writing the response
// may take. Uploads get minutes, since a snapshot can be tens of megabytes
// on a slow uplink; everything else gets seconds. Slow-header attacks are
// cut off earlier by the server's ReadHeaderTimeout.
func deadlines(read, write time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		now := time.Now()
		_ = rc.SetReadDeadline(now.Add(read))
		if write > 0 {
			_ = rc.SetWriteDeadline(now.Add(write))
		}
		next(w, r)
	}
}

// cors lets the configured browser origins read the public API. Writes
// never get CORS headers: only the CLI writes, and it is not a browser.
func (a *API) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && len(a.corsOrigins) > 0 {
			w.Header().Add("Vary", "Origin")
			switch {
			case slices.Contains(a.corsOrigins, "*"):
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case slices.Contains(a.corsOrigins, origin):
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		}
		next(w, r)
	}
}

// preflight answers CORS preflight requests for the read API.
func (a *API) preflight(w http.ResponseWriter, r *http.Request) {
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
}

// readLimit applies the per-client budget for viewer requests.
func (a *API) readLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if per := a.limits.ReadsPerMinute; per > 0 {
			key := "read:" + clientKey(clientAddr(r.Context()))
			if ok, wait := a.limiter.Allow(r.Context(), key, per, time.Minute); !ok {
				retryAfter(w, wait)
				fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests. Slow down and try again shortly.", nil)
				return
			}
		}
		next(w, r)
	}
}
