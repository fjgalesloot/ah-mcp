package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mrserzhan/ah-mcp/tools"
)

const (
	defaultAHSite   = "nl"
	ahClientVersion = "9.28"
	ahUserAgent     = "Appie/9.28 (iPhone17,3; iPhone; CPU OS 26_1 like Mac OS X)"
	oauthTimeout    = 5 * time.Minute
	tokenRefreshBuf = 60 * time.Second

	// flowCookieName carries the flow secret for requests the login page makes
	// outside the secret prefix (see StartOAuthFlow).
	flowCookieName = "ah_mcp_flow"

	loginSuccessHTML = `<!DOCTYPE html>
<html><head><title>Login Successful</title></head>
<body style="font-family:system-ui;max-width:500px;margin:80px auto;text-align:center">
<h1>Login successful!</h1>
<p>You can close this tab and return to your AI assistant.</p>
<script>setTimeout(function(){window.close()},1000)</script>
</body></html>`
)

// ahHTTPClient is used for the auth calls this package makes directly
// (token exchange and refresh). It carries a timeout so a hung AH endpoint
// cannot block a tool call indefinitely.
var ahHTTPClient = &http.Client{Timeout: 30 * time.Second}

// tokenFile is the on-disk format — matches appie-go's internal config type
// so that appie.NewWithConfig can load tokens written by our OAuth flow.
type tokenFile struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	MemberID     string    `json:"member_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// tokenResponse matches the AH OAuth token API response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	MemberID     string `json:"member_id,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
}

func ahSite() string {
	site := strings.ToLower(strings.TrimSpace(os.Getenv("AH_SITE")))
	if site == "be" {
		return "be"
	}
	return defaultAHSite
}

func ahAPIBase() string {
	return "https://api.ah." + ahSite()
}

func ahClientID() string {
	if ahSite() == "be" {
		return "appie-be-ios"
	}
	return "appie-ios"
}

func ahLoginBase() string {
	return "https://login.ah." + ahSite()
}

func ahApplication() string {
	if ahSite() == "be" {
		return "AHBEWEBSHOP"
	}
	return "AHWEBSHOP"
}

// TokensPath returns the path for storing OAuth tokens.
// Override with AH_TOKENS_PATH env var; otherwise uses XDG-compliant location.
func TokensPath() string {
	if p := os.Getenv("AH_TOKENS_PATH"); p != "" {
		return p
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = os.TempDir()
	}
	return filepath.Join(configDir, "ah-mcp", "tokens.json")
}

// LoadTokens reads tokens from disk. Returns nil (no error) if file does not exist.
func LoadTokens(path string) (*tokenFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("parse tokens: %w", err)
	}
	return &tf, nil
}

// SaveTokens writes tokens atomically (temp file + rename) with mode 0600.
func SaveTokens(path string, tf *tokenFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	// Write to temp file in same directory, then rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp tokens: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tokens: %w", err)
	}
	return nil
}

// IsAuthenticated returns true if a non-empty refresh token is stored on disk.
func IsAuthenticated(path string) bool {
	tf, err := LoadTokens(path)
	if err != nil || tf == nil {
		return false
	}
	return tf.RefreshToken != ""
}

// refreshMu serialises token refreshes. Without it two concurrent tool calls
// can both POST the same refresh token; AH rotates refresh tokens, so the
// loser of that race would persist a token the server has already retired.
var refreshMu sync.Mutex

// RefreshIfNeeded refreshes the access token if it expires within tokenRefreshBuf.
// Saves updated tokens on success. The bool reports whether a refresh actually
// happened, so the caller knows the in-memory appie client is now stale.
func RefreshIfNeeded(ctx context.Context, path string) (bool, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	tf, err := LoadTokens(path)
	if err != nil {
		return false, fmt.Errorf("load tokens for refresh: %w", err)
	}
	if tf == nil || tf.RefreshToken == "" {
		return false, fmt.Errorf("no refresh token available")
	}

	if !tf.ExpiresAt.IsZero() && time.Until(tf.ExpiresAt) > tokenRefreshBuf {
		return false, nil // still valid
	}

	reqBody := map[string]string{
		"clientId":     ahClientID(),
		"refreshToken": tf.RefreshToken,
	}
	var tok tokenResponse
	if err := doAHPost(ctx, "/mobile-auth/v1/auth/token/refresh", reqBody, &tok); err != nil {
		return false, fmt.Errorf("refresh token: %w", err)
	}

	tf.AccessToken = tok.AccessToken
	tf.RefreshToken = tok.RefreshToken
	if tok.ExpiresIn > 0 {
		tf.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	if err := SaveTokens(path, tf); err != nil {
		return false, err
	}
	tools.LogInfo("auth", "token_refreshed expires_at=%s", tf.ExpiresAt.UTC().Format(time.RFC3339))
	return true, nil
}

// flowSecret returns an unguessable path segment used to scope the OAuth
// proxy. Without it the proxy is an open relay to AH's login host and its
// /callback accepts an authorization code from anyone who can reach the port
// (AH's flow carries no state parameter, so that is a login-CSRF: an attacker
// can bind *their* AH account to this server).
func flowSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate flow secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StartOAuthFlow starts a temporary reverse-proxy HTTP server on callbackPort,
// rewrites AH's appie:// redirect to the local callback handler, exchanges
// the auth code for tokens and saves them.
//
// Everything is mounted under a random /<secret>/ prefix that is only ever
// disclosed through the returned login URL, which in turn only reaches the
// user over the authenticated MCP channel.
//
// The proxy approach is necessary because the AH OAuth server only accepts
// redirect_uri=appie://login-exit (a custom iOS URL scheme). The proxy
// intercepts this redirect and converts it to an HTTP callback we can receive.
func StartOAuthFlow(callbackHost string, callbackPort int, tokensPath string, remote bool) (*tools.OAuthFlow, error) {
	secret, err := flowSecret()
	if err != nil {
		return nil, err
	}

	// Local logins only ever come from this machine's browser, so do not put
	// the proxy on the network. Remote deployments sit behind a reverse proxy
	// and must accept forwarded traffic.
	host := "127.0.0.1"
	if remote {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, callbackPort)
	listener, listenErr := net.Listen("tcp", addr)
	if listenErr != nil {
		return nil, fmt.Errorf("start OAuth server on %s: %w", addr, listenErr)
	}

	target, err := url.Parse(ahLoginBase())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("parse AH login base: %w", err)
	}

	prefix := "/" + secret
	localOrigin := strings.TrimSuffix(callbackHost, "/") + prefix
	// Cookies must keep their Secure flag unless the browser will be talking
	// to us over plain HTTP, which is only the case for local logins.
	insecureOrigin := strings.HasPrefix(strings.ToLower(localOrigin), "http://")

	tools.LogInfo("auth", "oauth_start site=%s login_host=%s api_base=%s bind=%s remote=%t", ahSite(), target.Host, ahAPIBase(), addr, remote)
	codeCh := make(chan string, 1)
	doneCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		tools.LogInfo("auth", "oauth_callback_received code_len=%d", len(code))
		select {
		case codeCh <- code:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, loginSuccessHTML)
	})

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Accept-Encoding")
			// The flow cookie is ours; AH has no business seeing it.
			dropCookie(req.Header, flowCookieName)
			// Rewrite Origin and Referer so AH's API doesn't reject the request
			// because it sees our proxy hostname instead of the AH login host.
			if origin := req.Header.Get("Origin"); origin != "" {
				req.Header.Set("Origin", target.Scheme+"://"+target.Host)
			}
			if referer := req.Header.Get("Referer"); referer != "" {
				req.Header.Set("Referer", strings.Replace(referer, localOrigin, target.Scheme+"://"+target.Host, 1))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return rewriteOAuthResponse(resp, localOrigin, target.Host, insecureOrigin)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			tools.LogWarn("auth", "proxy_error method=%s path=%s err=%v", r.Method, r.URL.Path, proxyErr)
			http.Error(w, "proxy error", http.StatusBadGateway)
		},
	}
	// AH's login page is a Next.js app that loads its scripts, styles and API
	// calls from root-relative paths (/login/_next/...), which fall outside the
	// secret prefix. Without them the page never hydrates and the login button
	// stays disabled. So entering through the secret prefix hands the browser a
	// cookie holding the secret, and root paths are proxied only when it is
	// present. The callback stays reachable under the prefix alone.
	mux.Handle(prefix+"/", http.StripPrefix(prefix, withFlowCookie(secret, !insecureOrigin, proxy)))
	mux.Handle("/", requireFlowCookie(secret, proxy))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(listener) }()

	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}

	// Build the login URL that the user must open (via our local proxy).
	loginURL := fmt.Sprintf(
		"%s/login?client_id=%s&response_type=code&redirect_uri=appie://login-exit",
		localOrigin, ahClientID(),
	)
	tools.LogInfo("auth", "oauth_login_url_ready site=%s client_id=%s login_base=%s", ahSite(), ahClientID(), ahLoginBase())

	cancelCh := make(chan struct{})
	var cancelOnce sync.Once

	// Wait for the code, exchange it, save tokens — all in background.
	go func() {
		defer shutdown()

		select {
		case code := <-codeCh:
			doneCh <- exchangeCodeAndSave(context.Background(), code, tokensPath)
		case <-cancelCh:
			doneCh <- fmt.Errorf("OAuth flow cancelled")
		case <-time.After(oauthTimeout):
			doneCh <- fmt.Errorf("OAuth flow timed out after 5 minutes")
		}
	}()

	return &tools.OAuthFlow{
		LoginURL: loginURL,
		Done:     doneCh,
		Cancel:   func() { cancelOnce.Do(func() { close(cancelCh) }) },
	}, nil
}

// withFlowCookie gives the browser a cookie proving it entered through the
// secret prefix, so requireFlowCookie lets its root-relative requests through.
func withFlowCookie(secret string, secure bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(flowCookieName); err != nil || c.Value != secret {
			http.SetCookie(w, &http.Cookie{
				Name:     flowCookieName,
				Value:    secret,
				Path:     "/",
				HttpOnly: true,
				Secure:   secure,
				SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// requireFlowCookie serves next only for requests carrying the flow secret in
// the flow cookie; everything else 404s, as if the path did not exist.
func requireFlowCookie(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(flowCookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(secret)) != 1 {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dropCookie removes the named cookie from the Cookie header. It works on the
// raw header rather than re-serialising parsed cookies, which would silently
// drop upstream cookies whose values Go considers invalid.
func dropCookie(h http.Header, name string) {
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if n, _, _ := strings.Cut(part, "="); strings.TrimSpace(n) == name {
				continue
			}
			kept = append(kept, part)
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// exchangeCodeAndSave exchanges an auth code for tokens and saves them.
func exchangeCodeAndSave(ctx context.Context, code, tokensPath string) error {
	reqBody := map[string]string{
		"clientId": ahClientID(),
		"code":     code,
	}
	var tok tokenResponse
	if err := doAHPost(ctx, "/mobile-auth/v1/auth/token", reqBody, &tok); err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}

	tf := &tokenFile{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		MemberID:     tok.MemberID,
	}
	if tok.ExpiresIn > 0 {
		tf.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return SaveTokens(tokensPath, tf)
}

// doAHPost posts JSON to an AH API path and decodes the response into result.
func doAHPost(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ahAPIBase()+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", ahUserAgent)
	req.Header.Set("x-client-name", ahClientID())
	req.Header.Set("x-client-version", ahClientVersion)
	req.Header.Set("x-application", ahApplication())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := ahHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		// The body of a failed auth call can echo tokens — report the status only.
		return fmt.Errorf("AH API error %d", resp.StatusCode)
	}
	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// rewriteOAuthResponse intercepts AH login responses and:
//   - rewrites appie:// Location redirects to localOrigin/callback
//   - strips security headers that block the proxy
//   - sanitizes cookies for HTTP use on localhost
//   - replaces appie:// and AH login URLs in HTML/JS/JSON bodies
func rewriteOAuthResponse(resp *http.Response, localOrigin, targetHost string, insecureOrigin bool) error {
	// Intercept server-side redirects to appie://
	loc := resp.Header.Get("Location")
	if strings.HasPrefix(loc, "appie://") {
		u, err := url.Parse(loc)
		if err != nil {
			return fmt.Errorf("parse appie URL %q: %w", loc, err)
		}
		resp.Header.Set("Location", fmt.Sprintf("%s/callback?%s", localOrigin, u.Query().Encode()))
		return nil
	}
	if strings.Contains(loc, targetHost) {
		resp.Header.Set("Location", strings.ReplaceAll(loc, "https://"+targetHost, localOrigin))
	}

	// Strip security headers that would break the proxy
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Strict-Transport-Security")
	resp.Header.Del("X-Frame-Options")

	// Sanitize cookies: strip Domain (it names the AH host, not ours) and
	// SameSite. Secure is only dropped when the browser reaches us over HTTP.
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, c := range cookies {
			resp.Header.Add("Set-Cookie", sanitizeCookie(c, insecureOrigin))
		}
	}

	// Only rewrite bodies for text content types
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") &&
		!strings.Contains(ct, "javascript") &&
		!strings.Contains(ct, "json") {
		return nil
	}

	body, err := readBody(resp)
	if err != nil {
		return err
	}
	body = bytes.ReplaceAll(body, []byte("appie://login-exit"), []byte(localOrigin+"/callback"))
	body = bytes.ReplaceAll(body, []byte("https://"+targetHost), []byte(localOrigin))

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// sanitizeCookie strips SameSite and Domain from a Set-Cookie value, and
// Secure too when the browser will reach the proxy over plain HTTP.
func sanitizeCookie(cookie string, insecureOrigin bool) string {
	parts := strings.Split(cookie, ";")
	out := parts[:1]
	for _, p := range parts[1:] {
		attr := strings.ToLower(strings.TrimSpace(p))
		if attr == "secure" && !insecureOrigin {
			out = append(out, p)
			continue
		}
		if attr == "secure" ||
			strings.HasPrefix(attr, "samesite") ||
			strings.HasPrefix(attr, "domain") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}

// readBody reads and returns the response body, handling gzip encoding.
func readBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	data, err := io.ReadAll(reader)
	resp.Body.Close()
	return data, err
}
