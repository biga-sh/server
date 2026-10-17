package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

var (
	errTooLarge            = errors.New("request body is too large")
	errUnsupportedEncoding = errors.New("unsupported Content-Encoding")
	errUnsupportedType     = errors.New("Content-Type must be application/json")
	errMalformed           = errors.New("request body is not valid JSON")
)

// readJSON decodes a request body into v, enforcing max on the decoded
// bytes: a small gzip body cannot expand past the limit, and a missing or
// lying Content-Length cannot smuggle a larger one through. An empty body
// leaves v untouched when allowEmpty is set.
func readJSON(w http.ResponseWriter, r *http.Request, max int64, v any, allowEmpty bool) error {
	if r.ContentLength > 0 || r.Header.Get("Content-Type") != "" {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			return errUnsupportedType
		}
	}
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return errUnsupportedEncoding
	}
	// The raw body can never legitimately exceed the decoded limit.
	if r.ContentLength > max {
		return errTooLarge
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, max)
	if encoding == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return bodyErr(err)
		}
		defer zr.Close()
		body = zr
	}

	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return bodyErr(err)
	}
	if int64(len(data)) > max {
		return errTooLarge
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if allowEmpty {
			return nil
		}
		return errMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s", errMalformed, jsonProblem(err))
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data after the JSON value", errMalformed)
	}
	return nil
}

func bodyErr(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return errTooLarge
	}
	return fmt.Errorf("%w: the body is truncated or corrupt", errMalformed)
}

// jsonProblem describes a decode error without echoing the input.
func jsonProblem(err error) string {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return fmt.Sprintf("syntax error at byte %d", syn.Offset)
	case errors.As(err, &typ):
		return fmt.Sprintf("field %q has the wrong type", typ.Field)
	default:
		return "malformed"
	}
}
