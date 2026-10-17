package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// acceptedEncodings is advertised on every JSON response (RFC 7694). The
// CLI compresses uploads only after seeing it.
const acceptedEncodings = "gzip"

// minGzipBytes is the smallest response worth compressing.
const minGzipBytes = 1400

type envelope struct {
	OK    bool      `json:"ok"`
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// respond writes data in the success envelope.
func respond(w http.ResponseWriter, r *http.Request, status int, data any) {
	writeJSON(w, r, status, envelope{OK: true, Data: data}, true)
}

// respondPrivate is respond for bodies holding a credential: never
// compressed, so a compression side channel (BREACH) has nothing to measure,
// and never cached.
func respondPrivate(w http.ResponseWriter, r *http.Request, status int, data any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, status, envelope{OK: true, Data: data}, false)
}

// fail writes the error envelope.
func fail(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="biga"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, status, envelope{Error: &apiError{Code: code, Message: message, Details: details}}, false)
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any, compressible bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, `{"ok":false,"error":{"code":"internal_error","message":"An unexpected error occurred."}}`, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Accept-Encoding", acceptedEncodings)
	if compressible {
		h.Add("Vary", "Accept-Encoding")
	}
	body := buf.Bytes()
	if compressible && buf.Len() >= minGzipBytes && acceptsGzip(r) {
		var z bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&z, gzip.BestSpeed)
		_, _ = zw.Write(body)
		if zw.Close() == nil {
			h.Set("Content-Encoding", "gzip")
			body = z.Bytes()
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for enc := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
			if strings.EqualFold(strings.TrimSpace(name), "gzip") && !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") {
				return true
			}
		}
	}
	return false
}

// retryAfter sets a Retry-After header in whole seconds, at least one.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int((d + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
}
