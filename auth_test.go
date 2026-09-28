package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrserzhan/ah-mcp/tools"
)

func TestSanitizeCookie(t *testing.T) {
	const in = "session=abc; Path=/; Domain=.login.ah.nl; Secure; HttpOnly; SameSite=None"

	// Over plain HTTP the browser would drop a Secure cookie entirely, so the
	// proxy has to strip it for local logins.
	insecure := sanitizeCookie(in, true)
	for _, banned := range []string{"Secure", "Domain", "SameSite"} {
		if strings.Contains(insecure, banned) {
			t.Fatalf("insecure origin: %q should not contain %q", insecure, banned)
		}
	}
	if !strings.Contains(insecure, "session=abc") || !strings.Contains(insecure, "HttpOnly") {
		t.Fatalf("insecure origin: %q lost the cookie value or HttpOnly", insecure)
	}

	// Over HTTPS the Secure flag must survive — dropping it would let the
	// cookie leak over a plaintext connection.
	secure := sanitizeCookie(in, false)
	if !strings.Contains(secure, "Secure") {
		t.Fatalf("secure origin: %q must keep Secure", secure)
	}
	if !strings.Contains(secure, "HttpOnly") {
		t.Fatalf("secure origin: %q must keep HttpOnly", secure)
	}
	if strings.Contains(secure, "Domain") || strings.Contains(secure, "SameSite") {
		t.Fatalf("secure origin: %q must still drop Domain and SameSite", secure)
	}
}

func newTestResponse(body, contentType string) *http.Response {
	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
	if contentType != "" {
		resp.Header.Set("Content-Type", contentType)
	}
	return resp
}

func TestRewriteOAuthResponseRedirect(t *testing.T) {
	resp := newTestResponse("", "")
	resp.Header.Set("Location", "appie://login-exit?code=THECODE&state=x")

	if err := rewriteOAuthResponse(resp, "http://localhost:9876/abc123", "login.ah.nl", true); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := resp.Header.Get("Location")
	if !strings.HasPrefix(got, "http://localhost:9876/abc123/callback?") {
		t.Fatalf("Location = %q, want the secret-scoped callback", got)
	}
	if !strings.Contains(got, "code=THECODE") {
		t.Fatalf("Location = %q, lost the authorization code", got)
	}
}

func TestRewriteOAuthResponseStripsSecurityHeaders(t *testing.T) {
	resp := newTestResponse("<html></html>", "text/html")
	resp.Header.Set("Content-Security-Policy", "default-src 'none'")
	resp.Header.Set("Strict-Transport-Security", "max-age=63072000")
	resp.Header.Set("X-Frame-Options", "DENY")

	if err := rewriteOAuthResponse(resp, "http://localhost:9876/abc123", "login.ah.nl", true); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, h := range []string{"Content-Security-Policy", "Strict-Transport-Security", "X-Frame-Options"} {
		if resp.Header.Get(h) != "" {
			t.Fatalf("%s should have been stripped", h)
		}
	}
}

func TestRewriteOAuthResponseBody(t *testing.T) {
	const localOrigin = "http://localhost:9876/abc123"
	body := `<a href="appie://login-exit">go</a><script>var u="https://login.ah.nl/x";</script>`
	resp := newTestResponse(body, "text/html; charset=utf-8")

	if err := rewriteOAuthResponse(resp, localOrigin, "login.ah.nl", true); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "appie://login-exit") {
		t.Fatalf("body still contains the appie scheme: %q", got)
	}
	if strings.Contains(got, "https://login.ah.nl") {
		t.Fatalf("body still points at the AH host: %q", got)
	}
	if !strings.Contains(got, localOrigin+"/callback") {
		t.Fatalf("body missing rewritten callback: %q", got)
	}
	if resp.ContentLength != int64(len(got)) {
		t.Fatalf("ContentLength = %d, want %d", resp.ContentLength, len(got))
	}
}

func TestRewriteOAuthResponseLeavesBinaryBodies(t *testing.T) {
	const body = "appie://login-exit"
	resp := newTestResponse(body, "image/png")
	if err := rewriteOAuthResponse(resp, "http://localhost:9876/abc", "login.ah.nl", true); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != body {
		t.Fatalf("binary body was rewritten: %q", out)
	}
}

func TestFlowSecretIsUniqueAndLong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := flowSecret()
		if err != nil {
			t.Fatalf("flowSecret: %v", err)
		}
		if len(s) < 32 {
			t.Fatalf("secret %q is too short to resist guessing", s)
		}
		if seen[s] {
			t.Fatalf("secret %q repeated", s)
		}
		seen[s] = true
	}
}

func TestTokensRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tokens.json")

	if tf, err := LoadTokens(path); err != nil || tf != nil {
		t.Fatalf("missing file should load as (nil, nil), got (%v, %v)", tf, err)
	}
	if IsAuthenticated(path) {
		t.Fatal("no token file must not count as authenticated")
	}

	want := &tokenFile{
		AccessToken:  "access",
		RefreshToken: "refresh",
		MemberID:     "m1",
		ExpiresAt:    time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := SaveTokens(path, want); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}

	got, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.MemberID != want.MemberID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	if !IsAuthenticated(path) {
		t.Fatal("a stored refresh token should count as authenticated")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("token file mode = %o, want 600", perm)
		}
	}

	// No temp file should survive the atomic write.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind after SaveTokens")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot find a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// The OAuth proxy must only serve its random prefix. Anything else would make
// it an open relay to AH's login host and let a stranger post an auth code.
func TestStartOAuthFlowScopesEverythingToTheSecret(t *testing.T) {
	port := freePort(t)
	host := "http://127.0.0.1:" + strconv.Itoa(port)

	flow, err := StartOAuthFlow(host, port, filepath.Join(t.TempDir(), "tokens.json"), false)
	if err != nil {
		t.Fatalf("StartOAuthFlow: %v", err)
	}
	defer flow.Cancel()

	if !strings.HasPrefix(flow.LoginURL, host+"/") {
		t.Fatalf("LoginURL = %q, want it under %q", flow.LoginURL, host)
	}
	secret := strings.TrimPrefix(flow.LoginURL, host+"/")
	secret = secret[:strings.Index(secret, "/")]
	if len(secret) < 32 {
		t.Fatalf("login URL secret %q is too short", secret)
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// Unprefixed paths must not reach AH and must not accept a code.
	for _, path := range []string{"/login", "/callback?code=stolen", "/", "/wrong-secret/callback?code=stolen"} {
		resp, err := client.Get(host + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404 (path is outside the secret prefix)", path, resp.StatusCode)
		}
	}

	// The real callback is reachable and reports a missing code.
	resp, err := client.Get(host + "/" + secret + "/callback")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without a code: status = %d, want 400", resp.StatusCode)
	}
}

// The login page loads its assets from root-relative paths, so those must be
// proxied — but only for a browser that came in through the secret URL.
func TestFlowCookieGatesRootPaths(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	var reached []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
	})
	mux := http.NewServeMux()
	mux.Handle("/"+secret+"/", http.StripPrefix("/"+secret, withFlowCookie(secret, false, next)))
	mux.Handle("/", requireFlowCookie(secret, next))

	// Entering through the secret URL sets the cookie.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+secret+"/login", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != flowCookieName || cookies[0].Value != secret || !cookies[0].HttpOnly {
		t.Fatalf("entry cookies = %+v, want one HttpOnly %s cookie holding the secret", cookies, flowCookieName)
	}

	cases := []struct {
		name   string
		cookie string
		want   int
	}{
		{"no cookie", "", http.StatusNotFound},
		{"wrong secret", flowCookieName + "=nope", http.StatusNotFound},
		{"valid cookie", flowCookieName + "=" + secret, http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/login/_next/static/chunks/app.js", nil)
		if tc.cookie != "" {
			req.Header.Set("Cookie", tc.cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	if want := []string{"/login", "/login/_next/static/chunks/app.js"}; strings.Join(reached, ",") != strings.Join(want, ",") {
		t.Fatalf("proxied paths = %v, want %v", reached, want)
	}
}

func TestDropCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", `a=1; `+flowCookieName+`=secret; b="x y"`)
	h.Add("Cookie", "c=3")
	dropCookie(h, flowCookieName)
	if got, want := h.Get("Cookie"), `a=1; b="x y"; c=3`; got != want || len(h.Values("Cookie")) != 1 {
		t.Fatalf("Cookie = %q, want %q", h.Values("Cookie"), want)
	}

	h = http.Header{"Cookie": {flowCookieName + "=secret"}}
	dropCookie(h, flowCookieName)
	if _, ok := h["Cookie"]; ok {
		t.Fatalf("Cookie header should be removed when only the flow cookie was present, got %q", h.Values("Cookie"))
	}
}

// Cancelling a flow must free the port so the next ah_login can bind it.
func TestStartOAuthFlowCancelReleasesPort(t *testing.T) {
	port := freePort(t)
	host := "http://127.0.0.1:" + strconv.Itoa(port)
	tokens := filepath.Join(t.TempDir(), "tokens.json")

	flow, err := StartOAuthFlow(host, port, tokens, false)
	if err != nil {
		t.Fatalf("first StartOAuthFlow: %v", err)
	}
	flow.Cancel()

	if err := <-flow.Done; err == nil {
		t.Fatal("cancelled flow should report an error on Done")
	}

	// The listener closes asynchronously during shutdown.
	var second *tools.OAuthFlow
	for i := 0; i < 50; i++ {
		f, err := StartOAuthFlow(host, port, tokens, false)
		if err == nil {
			second = f
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("port was never released after Cancel")
	}
	second.Cancel()
}
