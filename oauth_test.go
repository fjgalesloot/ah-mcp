package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testAudience = "ah-mcp-client-id"

var (
	testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	rsaKeyOnce sync.Once
	rsaKeys    [2]*rsa.PrivateKey
)

// testRSAKeys returns two 2048-bit keys, generated once per test binary.
func testRSAKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	rsaKeyOnce.Do(func() {
		for i := range rsaKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			rsaKeys[i] = k
		}
	})
	return rsaKeys[0], rsaKeys[1]
}

// fakeAuthentik mimics an Authentik OAuth2 provider: a per-application issuer
// path with OpenID discovery and a JWKS endpoint underneath it.
type fakeAuthentik struct {
	srv    *httptest.Server
	issuer string

	mu       sync.Mutex
	keys     []map[string]string
	jwksHits int
}

func newFakeAuthentik(t *testing.T) *fakeAuthentik {
	t.Helper()
	fa := &fakeAuthentik{}
	mux := http.NewServeMux()
	mux.HandleFunc("/application/o/ah-mcp/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   fa.issuer,
			"jwks_uri": fa.srv.URL + "/application/o/ah-mcp/jwks/",
		})
	})
	mux.HandleFunc("/application/o/ah-mcp/jwks/", func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		fa.jwksHits++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": fa.keys})
	})
	fa.srv = httptest.NewServer(mux)
	t.Cleanup(fa.srv.Close)
	fa.issuer = fa.srv.URL + "/application/o/ah-mcp/"
	return fa
}

func (fa *fakeAuthentik) setKeys(keys ...map[string]string) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.keys = keys
}

func (fa *fakeAuthentik) hits() int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.jwksHits
}

func (fa *fakeAuthentik) verifier() *jwtVerifier {
	v := newJWTVerifier(fa.issuer, testAudience, fa.srv.Client())
	v.now = func() time.Time { return testNow }
	return v
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, k *rsa.PrivateKey) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes()),
	}
}

func ecJWK(kid string, k *ecdsa.PrivateKey) map[string]string {
	return map[string]string{
		"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(k.X.FillBytes(make([]byte, 32))), "y": b64(k.Y.FillBytes(make([]byte, 32))),
	}
}

// validClaims are what Authentik puts in an access token for this client.
func validClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer,
		"aud": testAudience,
		"sub": "user-1",
		"iat": testNow.Add(-time.Minute).Unix(),
		"exp": testNow.Add(5 * time.Minute).Unix(),
	}
}

func sign(t *testing.T, method jwt.SigningMethod, kid string, key any, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestJWTVerifier(t *testing.T) {
	signer, other := testRSAKeys(t)
	fa := newFakeAuthentik(t)
	fa.setKeys(rsaJWK("k1", signer))

	with := func(mut func(jwt.MapClaims)) jwt.MapClaims {
		c := validClaims(fa.issuer)
		mut(c)
		return c
	}

	tests := []struct {
		name    string
		token   string
		wantErr error // nil means the token must be accepted
	}{
		{"valid", sign(t, jwt.SigningMethodRS256, "k1", signer, validClaims(fa.issuer)), nil},
		{"valid with audience list", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["aud"] = []string{"something-else", testAudience}
		})), nil},
		{"expired", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["exp"] = testNow.Add(-time.Minute).Unix()
		})), jwt.ErrTokenExpired},
		{"expired within leeway", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["exp"] = testNow.Add(-10 * time.Second).Unix()
		})), nil},
		{"missing exp", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			delete(c, "exp")
		})), jwt.ErrTokenRequiredClaimMissing},
		{"wrong audience", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["aud"] = "another-client"
		})), jwt.ErrTokenInvalidAudience},
		{"missing audience", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			delete(c, "aud")
		})), jwt.ErrTokenRequiredClaimMissing},
		{"wrong issuer", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["iss"] = "https://evil.example.com/application/o/ah-mcp/"
		})), jwt.ErrTokenInvalidIssuer},
		{"not yet valid", sign(t, jwt.SigningMethodRS256, "k1", signer, with(func(c jwt.MapClaims) {
			c["nbf"] = testNow.Add(time.Hour).Unix()
		})), jwt.ErrTokenNotValidYet},
		{"signed by another key", sign(t, jwt.SigningMethodRS256, "k1", other, validClaims(fa.issuer)),
			jwt.ErrTokenSignatureInvalid},
		{"unknown kid", sign(t, jwt.SigningMethodRS256, "k2", signer, validClaims(fa.issuer)),
			jwt.ErrTokenUnverifiable},
		// An HS256 token "signed" with the public key is the classic
		// algorithm-confusion forgery.
		{"hmac algorithm", sign(t, jwt.SigningMethodHS256, "k1", signer.N.Bytes(), validClaims(fa.issuer)),
			jwt.ErrTokenSignatureInvalid},
		{"alg none", sign(t, jwt.SigningMethodNone, "k1", jwt.UnsafeAllowNoneSignatureType, validClaims(fa.issuer)),
			jwt.ErrTokenSignatureInvalid},
		{"garbage", "not.a.jwt", jwt.ErrTokenMalformed},
	}
	v := fa.verifier()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tc.token)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Verify: %v, want success", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestJWTVerifierES256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fa := newFakeAuthentik(t)
	fa.setKeys(ecJWK("ec1", key))

	tok := sign(t, jwt.SigningMethodES256, "ec1", key, validClaims(fa.issuer))
	if _, err := fa.verifier().Verify(context.Background(), tok); err != nil {
		t.Fatalf("Verify ES256: %v", err)
	}
}

// A key published for one algorithm must not verify tokens using another.
func TestJWTVerifierKeyAlgMismatch(t *testing.T) {
	signer, _ := testRSAKeys(t)
	fa := newFakeAuthentik(t)
	fa.setKeys(rsaJWK("k1", signer)) // alg RS256

	tok := sign(t, jwt.SigningMethodPS256, "k1", signer, validClaims(fa.issuer))
	if _, err := fa.verifier().Verify(context.Background(), tok); err == nil {
		t.Fatal("PS256 token accepted with an RS256 key")
	}
}

// Authentik rotating its signing key must not need an ah-mcp restart, but
// tokens with made-up kids must not turn the verifier into a JWKS hammer.
func TestJWTVerifierKeyRotation(t *testing.T) {
	oldKey, newKey := testRSAKeys(t)
	fa := newFakeAuthentik(t)
	fa.setKeys(rsaJWK("old", oldKey))

	now := testNow
	v := fa.verifier()
	v.now = func() time.Time { return now }

	claims := validClaims(fa.issuer)
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, "old", oldKey, claims)); err != nil {
		t.Fatalf("old key: %v", err)
	}
	if fa.hits() != 1 {
		t.Fatalf("JWKS hits = %d, want 1", fa.hits())
	}

	fa.setKeys(rsaJWK("old", oldKey), rsaJWK("new", newKey))
	rotated := sign(t, jwt.SigningMethodRS256, "new", newKey, claims)

	// Within the refresh floor an unknown kid is rejected without a refetch.
	now = testNow.Add(10 * time.Second)
	if _, err := v.Verify(context.Background(), rotated); err == nil {
		t.Fatal("unknown kid accepted before refresh")
	}
	if fa.hits() != 1 {
		t.Fatalf("JWKS hits = %d, want 1 (refresh must be rate limited)", fa.hits())
	}

	now = testNow.Add(jwksMinRefresh + time.Second)
	if _, err := v.Verify(context.Background(), rotated); err != nil {
		t.Fatalf("rotated key after refresh: %v", err)
	}
	if fa.hits() != 2 {
		t.Fatalf("JWKS hits = %d, want 2", fa.hits())
	}
}

func TestJWTVerifierIssuerMismatch(t *testing.T) {
	signer, _ := testRSAKeys(t)
	fa := newFakeAuthentik(t)
	fa.setKeys(rsaJWK("k1", signer))

	// Authentik issuers end in a slash; leaving it off must fail loudly.
	v := newJWTVerifier(strings.TrimSuffix(fa.issuer, "/"), testAudience, fa.srv.Client())
	v.now = func() time.Time { return testNow }
	_, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, "k1", signer, validClaims(fa.issuer)))
	if err == nil || !strings.Contains(err.Error(), "reports issuer") {
		t.Fatalf("Verify error = %v, want discovery issuer mismatch", err)
	}
}

func TestParseJWKRejectsWeakKeys(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rsaJWK("small", small))
	if _, err := parseJWK(raw); err == nil {
		t.Fatal("1024-bit RSA key accepted")
	}

	offCurve, _ := json.Marshal(map[string]string{
		"kty": "EC", "kid": "bad", "crv": "P-256",
		"x": b64(make([]byte, 32)), "y": b64(append(make([]byte, 31), 1)),
	})
	if _, err := parseJWK(offCurve); err == nil {
		t.Fatal("EC point off the curve accepted")
	}

	enc, _ := json.Marshal(map[string]string{"kty": "RSA", "kid": "enc", "use": "enc"})
	if k, err := parseJWK(enc); k != nil || err != nil {
		t.Fatalf("encryption key = %v, %v; want skipped", k, err)
	}
}

// oauthAuthenticator builds an authenticator accepting both the static token
// and JWTs from fa, as main does when both are configured.
func oauthAuthenticator(fa *fakeAuthentik) *authenticator {
	return &authenticator{
		token:               testToken,
		jwt:                 fa.verifier(),
		resourceMetadataURL: "https://ah.example.com" + protectedResourcePath,
	}
}

func TestAuthMiddlewareOAuth(t *testing.T) {
	signer, _ := testRSAKeys(t)
	fa := newFakeAuthentik(t)
	fa.setKeys(rsaJWK("k1", signer))

	valid := sign(t, jwt.SigningMethodRS256, "k1", signer, validClaims(fa.issuer))
	expiredClaims := validClaims(fa.issuer)
	expiredClaims["exp"] = testNow.Add(-time.Hour).Unix()
	expired := sign(t, jwt.SigningMethodRS256, "k1", signer, expiredClaims)
	wrongAudClaims := validClaims(fa.issuer)
	wrongAudClaims["aud"] = "another-client"
	wrongAud := sign(t, jwt.SigningMethodRS256, "k1", signer, wrongAudClaims)

	const metadata = `resource_metadata="https://ah.example.com/.well-known/oauth-protected-resource"`
	tests := []struct {
		name      string
		header    string
		query     string
		want      int
		challenge string // expected WWW-Authenticate; "" means none
	}{
		{"static token header", "Bearer " + testToken, "", http.StatusOK, ""},
		{"static token query", "", testToken, http.StatusOK, ""},
		{"valid JWT", "Bearer " + valid, "", http.StatusOK, ""},
		{"valid JWT lowercase scheme", "bearer " + valid, "", http.StatusOK, ""},
		{"expired JWT", "Bearer " + expired, "", http.StatusUnauthorized, `Bearer error="invalid_token", ` + metadata},
		{"wrong audience JWT", "Bearer " + wrongAud, "", http.StatusUnauthorized, `Bearer error="invalid_token", ` + metadata},
		{"JWT in query string", "", valid, http.StatusUnauthorized, `Bearer error="invalid_token", ` + metadata},
		{"basic auth", "Basic Zm9vOmJhcg==", "", http.StatusUnauthorized, `Bearer error="invalid_token", ` + metadata},
		{"no credentials", "", "", http.StatusUnauthorized, "Bearer " + metadata},
	}
	for _, mw := range []struct {
		name string
		wrap func(*authenticator, http.Handler) http.Handler
	}{
		{"streamable-http", simpleAuthMiddleware},
		{"sse", tokenAuthMiddleware},
	} {
		h := mw.wrap(oauthAuthenticator(fa), okHandler())
		for _, tc := range tests {
			t.Run(mw.name+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/mcp?token="+tc.query, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d", rec.Code, tc.want)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != tc.challenge {
					t.Fatalf("WWW-Authenticate = %q, want %q", got, tc.challenge)
				}
			})
		}
	}
}

func TestWrapHandlerServesProtectedResourceMetadata(t *testing.T) {
	fa := newFakeAuthentik(t)
	auth := newAuthenticator(testToken, &oauthConfig{Issuer: fa.issuer, Audience: testAudience}, "https://ah.example.com/")
	h := wrapHandler(okHandler(), auth, "https://ah.example.com/", 3000, false)

	for path, resource := range map[string]string{
		"/.well-known/oauth-protected-resource":     "https://ah.example.com",
		"/.well-known/oauth-protected-resource/mcp": "https://ah.example.com/mcp",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://inspector.example.com") // public: no origin check
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 without credentials", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: Content-Type = %q", path, ct)
		}
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			BearerMethods        []string `json:"bearer_methods_supported"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if doc.Resource != resource {
			t.Fatalf("%s: resource = %q, want %q", path, doc.Resource, resource)
		}
		if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != fa.issuer {
			t.Fatalf("%s: authorization_servers = %v, want [%s]", path, doc.AuthorizationServers, fa.issuer)
		}
		if len(doc.BearerMethods) != 1 || doc.BearerMethods[0] != "header" {
			t.Fatalf("%s: bearer_methods_supported = %v, want [header]", path, doc.BearerMethods)
		}
	}

	// The health probe stays open with OAuth enabled too.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want 200 without credentials", healthPath, rec.Code)
	}

	// The MCP endpoint itself still requires credentials and points at the metadata.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/mcp status = %d, want 401", rec.Code)
	}
	want := `Bearer resource_metadata="https://ah.example.com/.well-known/oauth-protected-resource"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// Without OAuth nothing about the existing static-token behaviour changes.
func TestWrapHandlerStaticTokenOnly(t *testing.T) {
	h := wrapHandler(okHandler(), newAuthenticator(testToken, nil, "https://ah.example.com"), "https://ah.example.com", 3000, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, protectedResourcePath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metadata status = %d, want 401 when OAuth is off", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want none without OAuth", got)
	}
}

func TestOAuthConfigFromEnv(t *testing.T) {
	tests := []struct {
		name, issuer, audience string
		wantCfg, wantErr       bool
	}{
		{"unset", "", "", false, false},
		{"both set", "https://auth.example.com/application/o/ah-mcp/", testAudience, true, false},
		{"loopback http", "http://127.0.0.1:9000/application/o/ah-mcp/", testAudience, true, false},
		{"issuer only", "https://auth.example.com/application/o/ah-mcp/", "", false, true},
		{"audience only", "", testAudience, false, true},
		{"plain http", "http://auth.example.com/application/o/ah-mcp/", testAudience, false, true},
		{"relative", "auth.example.com", testAudience, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AH_MCP_OAUTH_ISSUER", tc.issuer)
			t.Setenv("AH_MCP_OAUTH_AUDIENCE", tc.audience)
			cfg, err := oauthConfigFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if (cfg != nil) != tc.wantCfg {
				t.Fatalf("cfg = %+v, wantCfg %t", cfg, tc.wantCfg)
			}
		})
	}
}
