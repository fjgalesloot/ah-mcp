package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "s3cret-token"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestTokenEqual(t *testing.T) {
	if !tokenEqual("abc", "abc") {
		t.Fatal("identical tokens must compare equal")
	}
	if tokenEqual("abc", "abd") || tokenEqual("abc", "ab") || tokenEqual("", "abc") {
		t.Fatal("differing tokens must not compare equal")
	}
}

func TestSimpleAuthMiddleware(t *testing.T) {
	h := simpleAuthMiddleware(&authenticator{token: testToken}, okHandler())

	tests := []struct {
		name   string
		header string
		query  string
		want   int
	}{
		{"no credentials", "", "", http.StatusUnauthorized},
		{"valid bearer", "Bearer " + testToken, "", http.StatusOK},
		{"wrong bearer", "Bearer nope", "", http.StatusUnauthorized},
		{"bearer without prefix", testToken, "", http.StatusUnauthorized},
		{"valid query token", "", testToken, http.StatusOK},
		{"wrong query token", "", "nope", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp?token="+tc.query, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestTokenAuthMiddlewareRejectsUnauthenticated(t *testing.T) {
	h := tokenAuthMiddleware(&authenticator{token: testToken}, okHandler())

	for _, path := range []string{"/sse", "/message?sessionId=made-up"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, rec.Code)
		}
	}
}

// An SSE session id is only usable for /message while its stream is open.
func TestTokenAuthMiddlewareSessionLifecycle(t *testing.T) {
	const sid = "abc123"

	streaming := make(chan struct{})
	release := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sse") {
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /message?sessionId=%s\n\n", sid)
			close(streaming)
			<-release
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := tokenAuthMiddleware(&authenticator{token: testToken}, inner)

	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		req := httptest.NewRequest(http.MethodGet, "/sse?token="+testToken, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-streaming

	// While the stream is open the session id authenticates /message posts.
	req := httptest.NewRequest(http.MethodPost, "/message?sessionId="+sid, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open stream: /message status = %d, want 200", rec.Code)
	}

	close(release)
	<-sseDone

	// Once the stream ends the id must no longer grant access, otherwise a
	// leaked session id is a permanent bypass.
	req = httptest.NewRequest(http.MethodPost, "/message?sessionId="+sid, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("closed stream: /message status = %d, want 401", rec.Code)
	}
}

func TestOriginMiddleware(t *testing.T) {
	allowed := []string{"http://localhost:3000", "https://claude.ai"}
	h := originMiddleware(allowed, okHandler())

	tests := []struct {
		name   string
		origin string
		want   int
	}{
		{"no origin (non-browser client)", "", http.StatusOK},
		{"allowed origin", "http://localhost:3000", http.StatusOK},
		{"allowed origin with trailing slash", "http://localhost:3000/", http.StatusOK},
		{"allowed origin different case", "HTTP://LOCALHOST:3000", http.StatusOK},
		{"rebinding attacker origin", "http://evil.example.com", http.StatusForbidden},
		{"same host wrong port", "http://localhost:3001", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestOriginMiddlewareWildcard(t *testing.T) {
	h := originMiddleware([]string{"*"}, okHandler())
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with wildcard", rec.Code)
	}
}

func TestAllowedOrigins(t *testing.T) {
	got := allowedOrigins("https://ah.example.com", 3000)
	want := "https://ah.example.com"
	found := false
	for _, o := range got {
		if o == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("allowedOrigins = %v, missing base URL origin %q", got, want)
	}

	t.Setenv("AH_MCP_ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com/")
	got = allowedOrigins("https://ah.example.com", 3000)
	if len(got) != 2 || got[0] != "https://a.example.com" || got[1] != "https://b.example.com" {
		t.Fatalf("override = %v, want the two configured origins trimmed", got)
	}
}

func TestBindAddr(t *testing.T) {
	// Local runs must not be reachable from the network.
	if got, err := bindAddr(3000, false); err != nil || got != "127.0.0.1:3000" {
		t.Fatalf("local bindAddr = %q, %v; want 127.0.0.1:3000", got, err)
	}
	if got, err := bindAddr(3000, true); err != nil || got != "0.0.0.0:3000" {
		t.Fatalf("remote bindAddr = %q, %v; want 0.0.0.0:3000", got, err)
	}

	t.Setenv("AH_MCP_BIND", "0.0.0.0")
	if got, err := bindAddr(8080, false); err != nil || got != "0.0.0.0:8080" {
		t.Fatalf("override bindAddr = %q, %v; want 0.0.0.0:8080", got, err)
	}

	t.Setenv("AH_MCP_BIND", "0.0.0.0:1234")
	if _, err := bindAddr(8080, false); err == nil {
		t.Fatal("AH_MCP_BIND with a port must be rejected")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:3000", "localhost:3000", "[::1]:3000"} {
		if !isLoopbackBind(addr) {
			t.Fatalf("%q should be loopback", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:3000", "192.168.1.10:3000", "[::]:3000"} {
		if isLoopbackBind(addr) {
			t.Fatalf("%q should not be loopback", addr)
		}
	}
}

func TestCheckTransportAuth(t *testing.T) {
	// A tokenless server on a public interface exposes the whole AH account.
	if err := checkTransportAuth("0.0.0.0:3000", "", false); err == nil {
		t.Fatal("public bind without a token must be refused")
	}
	if err := checkTransportAuth("0.0.0.0:3000", testToken, false); err != nil {
		t.Fatalf("public bind with a token must be allowed: %v", err)
	}
	if err := checkTransportAuth("0.0.0.0:3000", "", true); err != nil {
		t.Fatalf("public bind with OAuth must be allowed: %v", err)
	}
	if err := checkTransportAuth("127.0.0.1:3000", "", false); err != nil {
		t.Fatalf("loopback bind without a token must warn, not fail: %v", err)
	}
}

func TestHealthzBypassesAuthAndOrigin(t *testing.T) {
	h := wrapHandler(okHandler(), &authenticator{token: testToken}, "", 3000, false)

	req := httptest.NewRequest(http.MethodGet, healthPath, nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q, want 200 \"ok\n\"", rec.Code, rec.Body.String())
	}

	// Everything else still needs the token.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/mcp without token = %d, want 401", rec.Code)
	}
}

func TestHealthcheckURL(t *testing.T) {
	tests := []struct {
		bind string
		want string
	}{
		{"", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0", "http://127.0.0.1:8080/healthz"},
		{"[::]", "http://127.0.0.1:8080/healthz"},
		{"10.0.0.5", "http://10.0.0.5:8080/healthz"},
		{"[::1]", "http://[::1]:8080/healthz"},
	}
	for _, tc := range tests {
		t.Run(tc.bind, func(t *testing.T) {
			t.Setenv("AH_MCP_BIND", tc.bind)
			if got := healthcheckURL(8080); got != tc.want {
				t.Fatalf("healthcheckURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeHealth(t *testing.T) {
	srv := httptest.NewServer(wrapHandler(okHandler(), &authenticator{token: testToken}, "", 3000, false))
	defer srv.Close()

	if err := probeHealth(srv.URL + healthPath); err != nil {
		t.Fatalf("probeHealth on healthy server: %v", err)
	}
	if err := probeHealth(srv.URL + "/mcp"); err == nil {
		t.Fatal("probeHealth must fail on a non-200 response")
	}
}
