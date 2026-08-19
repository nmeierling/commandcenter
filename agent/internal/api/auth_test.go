package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth(t.TempDir(), "127.0.0.1:47123", []string{"http://localhost:5173"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func guarded(a *Auth) http.Handler {
	return a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
}

// The threat this protects against: a page on any website the developer visits
// can issue requests to 127.0.0.1. Without a token, that page could start and
// stop their projects.
func TestTokenIsRequired(t *testing.T) {
	a := newTestAuth(t)
	h := guarded(a)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:47123/api/projects/x/start", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}

	req.Header.Set("Authorization", "Bearer "+a.Token())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid token: got %d, want 200", rec.Code)
	}

	req.Header.Set("Authorization", "Bearer "+a.Token()+"x")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}
}

func TestTokenAcceptedFromQueryForEventSource(t *testing.T) {
	a := newTestAuth(t)
	// EventSource cannot set headers, so SSE endpoints take the token in the
	// query string.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:47123/api/events?token="+a.Token(), nil)
	rec := httptest.NewRecorder()
	guarded(a).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
}

// DNS rebinding: an attacker points evil.example at 127.0.0.1, so the browser
// considers their page same-origin with us and sends no Origin header at all.
// The Host header is what gives it away.
func TestHostHeaderMustBeLoopback(t *testing.T) {
	a := newTestAuth(t)
	h := guarded(a)

	for _, host := range []string{"evil.example", "evil.example:47123", "192.168.1.10:47123"} {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+a.Token())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %q: got %d, want 421", host, rec.Code)
		}
	}

	for _, host := range []string{"127.0.0.1:47123", "localhost:47123", "[::1]:47123", "localhost"} {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+a.Token())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: got %d, want 200", host, rec.Code)
		}
	}
}

func TestUnknownOriginIsRejectedBeforeTheToken(t *testing.T) {
	a := newTestAuth(t)
	h := guarded(a)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:47123/api/projects/x/start", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer "+a.Token())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403 even with a valid token", rec.Code)
	}
}

func TestKnownOriginsAreAllowed(t *testing.T) {
	a := newTestAuth(t)
	h := guarded(a)

	for _, origin := range []string{
		"http://127.0.0.1:47123",
		"http://localhost:47123",
		"http://localhost:5173", // the Vite dev server, passed in explicitly
	} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:47123/api/projects", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Authorization", "Bearer "+a.Token())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("origin %q: got %d, want 200", origin, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %q: CORS header = %q", origin, got)
		}
	}
}

func TestNoTokenModeStillChecksOriginAndHost(t *testing.T) {
	a, err := NewAuth(t.TempDir(), "127.0.0.1:47123", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	h := guarded(a)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:47123/api/projects", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("--no-token should allow same-origin requests, got %d", rec.Code)
	}

	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("--no-token must still reject foreign origins, got %d", rec.Code)
	}
}

func TestTokenPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := NewAuth(dir, "127.0.0.1:47123", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAuth(dir, "127.0.0.1:47123", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Otherwise every agent restart would invalidate the browser's saved token.
	if first.Token() != second.Token() {
		t.Error("token should be reused from disk")
	}
	if len(first.Token()) < 32 {
		t.Errorf("token is too short: %d chars", len(first.Token()))
	}
}

func TestURLIncludesToken(t *testing.T) {
	a := newTestAuth(t)
	want := "http://127.0.0.1:47123/?token=" + a.Token()
	if got := a.URL("127.0.0.1:47123"); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}
