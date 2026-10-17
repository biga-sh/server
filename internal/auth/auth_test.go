package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	t.Parallel()
	token, hash := NewToken()
	if !strings.HasPrefix(token, TokenPrefix) || len(token) < 40 {
		t.Fatalf("token %q looks weak", token)
	}
	if !CheckToken(token, hash) {
		t.Error("token does not match its own hash")
	}
	other, _ := NewToken()
	if other == token {
		t.Error("two tokens are equal")
	}
	if CheckToken(other, hash) {
		t.Error("a different token matched")
	}
	if CheckToken("", hash) || CheckToken(token, nil) || CheckToken(token, []byte("short")) {
		t.Error("empty or malformed input matched")
	}
}

func TestKeys(t *testing.T) {
	t.Parallel()
	if NewKeys(nil).Required() {
		t.Error("no keys should mean no requirement")
	}
	a, b := NewKey(), NewKey()
	keys := NewKeys([]string{a, b})
	if !keys.Required() || !keys.Valid(a) || !keys.Valid(b) {
		t.Error("configured keys should be valid")
	}
	if keys.Valid("") || keys.Valid(a+"x") || keys.Valid(NewKey()) {
		t.Error("unknown key accepted")
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()
	for header, want := range map[string]string{
		"":             "",
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"Basic abc":    "",
		"Bearer":       "",
		"Bearerabc":    "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
