// Package httpapi serves the session API the CLI and the viewer use.
//
// Reads are public: holding a session's ID is the permission to watch it.
// Writes need the session's owner token, and an access key when the server
// is configured with any. Every write is authorized before its body is read.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/config"
	"github.com/biga-sh/server/internal/events"
	"github.com/biga-sh/server/internal/ratelimit"
	"github.com/biga-sh/server/internal/service"
)

// Options configures the API.
type Options struct {
	Service *service.Service
	Events  *events.Hub
	Config  config.Config
	Limiter ratelimit.Limiter
	Log     *slog.Logger
	// Ready reports whether the server's dependencies answer.
	Ready func(context.Context) error
	// Done is closed when the server shuts down, ending live streams.
	Done <-chan struct{}
}

// API is the HTTP handler.
type API struct {
	svc         *service.Service
	hub         *events.Hub
	keys        *auth.Keys
	limiter     ratelimit.Limiter
	clients     clientResolver
	limits      config.Limits
	corsOrigins []string
	corp        string
	cronSecret  string
	tls         bool
	log         *slog.Logger
	ready       func(context.Context) error
	streams     *streamGate
	done        <-chan struct{}

	mux *http.ServeMux
}

// Fixed rate limits. Checkpoints are frequent (a burst of saves can flush
// every ~120 ms), so their budget is high; session creation is keyed by
// client to blunt mass session creation on a zero-account service.
var (
	limitSession    = rateLimit{60, time.Hour}
	limitSnapshot   = rateLimit{15, time.Minute}
	limitCheckpoint = rateLimit{240, time.Minute}
	limitHeartbeat  = rateLimit{30, time.Minute}
	limitOwnerRead  = rateLimit{30, time.Minute}
	// limitAuthFailures caps wrong tokens and keys per client.
	limitAuthFailures = rateLimit{30, 10 * time.Minute}
)

type rateLimit struct {
	n      int
	window time.Duration
}

// Request deadlines.
const (
	readTimeout   = 30 * time.Second
	writeTimeout  = 60 * time.Second
	uploadTimeout = 6 * time.Minute
)

// New returns the API handler.
func New(o Options) *API {
	a := &API{
		svc:         o.Service,
		hub:         o.Events,
		keys:        auth.NewKeys(o.Config.AccessKeys),
		limiter:     o.Limiter,
		clients:     clientResolver{trusted: o.Config.TrustedProxies},
		limits:      o.Config.Limits,
		corsOrigins: o.Config.CORSOrigins,
		corp:        "same-site",
		cronSecret:  o.Config.CronSecret,
		tls:         o.Config.TLS(),
		log:         o.Log,
		ready:       o.Ready,
		streams:     newStreamGate(o.Config.Limits.Streams, o.Config.Limits.StreamsPerClient),
		done:        o.Done,
		mux:         http.NewServeMux(),
	}
	if slices.Contains(a.corsOrigins, "*") {
		a.corp = "cross-origin"
	}
	a.routes()
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.base(a.mux).ServeHTTP(w, r)
}

func (a *API) routes() {
	m := a.mux
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return deadlines(readTimeout, writeTimeout, a.cors(a.readLimit(h)))
	}
	write := func(timeout time.Duration, h http.HandlerFunc) http.HandlerFunc {
		return deadlines(timeout, timeout+time.Minute, h)
	}

	// Written by the CLI.
	m.HandleFunc("PUT /api/sessions/{id}", write(readTimeout, a.putSession))
	m.HandleFunc("DELETE /api/sessions/{id}", write(readTimeout, a.endSession))
	m.HandleFunc("POST /api/sessions/{id}/snapshot", write(uploadTimeout, a.snapshot))
	m.HandleFunc("POST /api/sessions/{id}/checkpoint", write(uploadTimeout, a.checkpoint))
	m.HandleFunc("POST /api/sessions/{id}/heartbeat", write(readTimeout, a.heartbeat))
	m.HandleFunc("GET /api/sessions/{id}/manifest", write(readTimeout, a.manifest))

	// Read by the viewer.
	m.HandleFunc("GET /api/sessions/{id}", read(a.getSession))
	m.HandleFunc("GET /api/sessions/{id}/state", read(a.state))
	m.HandleFunc("GET /api/sessions/{id}/checkpoints", read(a.checkpoints))
	m.HandleFunc("GET /api/sessions/{id}/checkpoints/{seq}", read(a.checkpointBySeq))
	m.HandleFunc("GET /api/sessions/{id}/file", read(a.file))
	m.HandleFunc("GET /api/sessions/{id}/file/history", read(a.fileHistory))
	m.HandleFunc("GET /api/sessions/{id}/stream", a.cors(a.readLimit(a.stream)))
	m.HandleFunc("OPTIONS /api/sessions/{path...}", a.cors(a.preflight))

	if a.cronSecret != "" {
		m.HandleFunc("GET /api/cron/cleanup", write(writeTimeout, a.cron))
	}
	m.HandleFunc("GET /healthz", a.healthz)
	m.HandleFunc("GET /readyz", a.readyz)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, http.StatusNotFound, "not_found", "No such endpoint.", nil)
	})
}
