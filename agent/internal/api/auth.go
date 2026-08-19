package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Auth guards the local API.
//
// "Bind to localhost" is not on its own a security boundary. Every page the
// developer visits can issue requests to http://127.0.0.1:<port> from their
// browser, and this API starts processes and runs configured shell commands. So
// three independent checks apply:
//
//  1. Token — a secret generated at startup, stored 0600. Cross-origin
//     attackers cannot read it, so they cannot forge an authorised request.
//  2. Origin — any browser-supplied Origin must be one we know. This blocks
//     simple (non-preflighted) cross-origin POSTs regardless of the token.
//  3. Host — the request's Host header must be a loopback name. This blocks
//     DNS rebinding, where an attacker points their own hostname at 127.0.0.1
//     so the browser treats us as same-origin.
type Auth struct {
	token          string
	allowedOrigins map[string]bool
	// Disabled turns off token checks entirely (--no-token). Origin and Host
	// checks still apply.
	Disabled bool
}

// NewAuth loads or creates the token file under dir.
func NewAuth(dir string, listenAddr string, devOrigins []string, disabled bool) (*Auth, error) {
	a := &Auth{allowedOrigins: map[string]bool{}, Disabled: disabled}

	for _, o := range selfOrigins(listenAddr) {
		a.allowedOrigins[o] = true
	}
	for _, o := range devOrigins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o != "" {
			a.allowedOrigins[o] = true
		}
	}

	if disabled {
		return a, nil
	}
	token, err := loadOrCreateToken(filepath.Join(dir, "token"))
	if err != nil {
		return nil, err
	}
	a.token = token
	return a, nil
}

// Token returns the current API token.
func (a *Auth) Token() string { return a.token }

func loadOrCreateToken(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(raw)); len(t) >= 32 {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return token, nil
}

// selfOrigins lists the origins the UI is served from for a given bind address.
func selfOrigins(listenAddr string) []string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil
	}
	return []string{
		"http://127.0.0.1:" + port,
		"http://localhost:" + port,
		"http://[::1]:" + port,
	}
}

// Middleware applies the Host, Origin and token checks to API routes.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.checkHost(r) {
			writeError(w, http.StatusMisdirectedRequest,
				"this agent only accepts requests addressed to localhost")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !a.allowedOrigins[strings.TrimRight(origin, "/")] {
			writeError(w, http.StatusForbidden, "origin "+origin+" is not allowed")
			return
		}
		// CORS for the Vite dev server, which runs on a different port.
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Devcc-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !a.checkToken(r) {
			writeError(w, http.StatusUnauthorized,
				"missing or invalid API token; open the URL printed by the agent, or run `devcc url`")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkHost rejects requests whose Host header is not a loopback name, which is
// how DNS rebinding attacks reach a localhost-bound service.
func (a *Auth) checkHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Names like "localhost.localdomain" are fine; anything else is not.
	return strings.HasSuffix(host, ".localhost")
}

func (a *Auth) checkToken(r *http.Request) bool {
	if a.Disabled {
		return true
	}
	presented := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		presented = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if presented == "" {
		presented = r.Header.Get("X-Devcc-Token")
	}
	if presented == "" {
		// EventSource cannot set headers, so SSE endpoints accept the token as
		// a query parameter. It only ever travels over loopback.
		presented = r.URL.Query().Get("token")
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) == 1
}

// URL returns the address a user should open, including the token.
func (a *Auth) URL(listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = "47123"
	}
	base := "http://127.0.0.1:" + port + "/"
	if a.Disabled || a.token == "" {
		return base
	}
	return base + "?token=" + url.QueryEscape(a.token)
}
