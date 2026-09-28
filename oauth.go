package main

// OAuth 2.1 resource-server support, following the MCP authorization spec
// (2025-06-18). ah-mcp does not issue tokens itself: it advertises an external
// authorization server (Authentik) through RFC 9728 protected resource
// metadata and validates the JWT access tokens that server signs.

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mrserzhan/ah-mcp/tools"
)

const (
	protectedResourcePath = "/.well-known/oauth-protected-resource"

	jwksCacheTTL     = time.Hour
	jwksMinRefresh   = time.Minute // floor between refetches for an unknown kid
	jwtLeeway        = 30 * time.Second
	oauthHTTPTimeout = 10 * time.Second
	maxOAuthDocBytes = 1 << 20
	minRSAKeyBits    = 2048
)

// allowedJWTAlgs lists the asymmetric algorithms accepted for access tokens.
// HMAC and "none" are deliberately absent: accepting them would let anyone who
// can read the public JWKS forge a token.
var allowedJWTAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

type oauthConfig struct {
	Issuer   string
	Audience string
}

// oauthConfigFromEnv reads AH_MCP_OAUTH_ISSUER and AH_MCP_OAUTH_AUDIENCE.
// It returns nil when OAuth is not configured.
func oauthConfigFromEnv() (*oauthConfig, error) {
	issuer := os.Getenv("AH_MCP_OAUTH_ISSUER")
	audience := os.Getenv("AH_MCP_OAUTH_AUDIENCE")
	if issuer == "" && audience == "" {
		return nil, nil
	}
	if issuer == "" || audience == "" {
		return nil, errors.New("AH_MCP_OAUTH_ISSUER and AH_MCP_OAUTH_AUDIENCE must be set together")
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("AH_MCP_OAUTH_ISSUER is not an absolute URL: %q", issuer)
	}
	// Signing keys are fetched from the issuer, so a plain-HTTP issuer lets
	// anyone on the path substitute their own keys and mint tokens.
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("AH_MCP_OAUTH_ISSUER must use https, got %q", issuer)
	}
	return &oauthConfig{Issuer: issuer, Audience: audience}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protectedResourceHandler serves RFC 9728 metadata pointing MCP clients at
// the authorization server. A path after the well-known prefix names the
// resource it describes (RFC 9728 §3.1), e.g. /.well-known/oauth-protected-resource/mcp.
func protectedResourceHandler(baseURL, issuer string) http.Handler {
	base := strings.TrimSuffix(baseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"resource":                 base + strings.TrimPrefix(r.URL.Path, protectedResourcePath),
			"authorization_servers":    []string{issuer},
			"bearer_methods_supported": []string{"header"},
		})
		w.Header().Set("Content-Type", "application/json")
		// Public metadata; browser-based MCP clients fetch it cross-origin.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(body)
	})
}

// isProtectedResourcePath reports whether path is the metadata document or a
// path-suffixed variant of it.
func isProtectedResourcePath(path string) bool {
	return path == protectedResourcePath || strings.HasPrefix(path, protectedResourcePath+"/")
}

// jwtVerifier validates access tokens signed by the configured issuer. The
// JWKS location comes from the issuer's OpenID discovery document; keys are
// cached and refetched hourly, or sooner when a token names an unknown kid
// (which is what a key rotation looks like from here).
type jwtVerifier struct {
	issuer   string
	audience string
	client   *http.Client
	now      func() time.Time

	mu          sync.Mutex
	jwksURL     string
	keys        map[string]*jwk
	fetchedAt   time.Time
	attemptedAt time.Time
}

func newJWTVerifier(issuer, audience string, client *http.Client) *jwtVerifier {
	if client == nil {
		client = &http.Client{Timeout: oauthHTTPTimeout}
	}
	return &jwtVerifier{issuer: issuer, audience: audience, client: client, now: time.Now}
}

// Verify checks the token's signature, issuer, audience and expiry.
func (v *jwtVerifier) Verify(ctx context.Context, raw string) (*jwt.RegisteredClaims, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(raw, claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			return v.key(ctx, kid, t.Method.Alg())
		},
		jwt.WithValidMethods(allowedJWTAlgs),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(jwtLeeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// key returns the public key for kid, refreshing the JWKS when it is stale or
// does not contain kid.
func (v *jwtVerifier) key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	k, found := v.lookup(kid)
	stale := now.Sub(v.fetchedAt) > jwksCacheTTL
	if (stale || !found) && now.Sub(v.attemptedAt) >= jwksMinRefresh {
		v.attemptedAt = now
		if err := v.refresh(ctx); err != nil {
			if !found {
				return nil, err
			}
			tools.LogWarn("oauth", "JWKS refresh failed, using cached keys: %v", err)
		} else {
			k, found = v.lookup(kid)
		}
	}
	if !found {
		return nil, fmt.Errorf("no signing key with kid %q", kid)
	}
	if k.Alg != "" && k.Alg != alg {
		return nil, fmt.Errorf("key %q is for %s, token uses %s", kid, k.Alg, alg)
	}
	return k.pub, nil
}

// lookup finds a key by kid. A token without a kid is only accepted when the
// set holds exactly one key, so there is no ambiguity about which one signed it.
func (v *jwtVerifier) lookup(kid string) (*jwk, bool) {
	if kid == "" {
		if len(v.keys) == 1 {
			for _, k := range v.keys {
				return k, true
			}
		}
		return nil, false
	}
	k, ok := v.keys[kid]
	return k, ok
}

func (v *jwtVerifier) refresh(ctx context.Context) error {
	if v.jwksURL == "" {
		u, err := v.discoverJWKS(ctx)
		if err != nil {
			return err
		}
		v.jwksURL = u
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	keys := make(map[string]*jwk, len(set.Keys))
	for _, raw := range set.Keys {
		k, err := parseJWK(raw)
		if err != nil {
			tools.LogWarn("oauth", "skipping JWKS key: %v", err)
			continue
		}
		if k != nil {
			keys[k.Kid] = k
		}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no usable signing keys")
	}
	v.keys = keys
	v.fetchedAt = v.now()
	return nil
}

// discoverJWKS reads the issuer's OpenID discovery document. Authentik serves
// it per provider at <issuer>/.well-known/openid-configuration.
func (v *jwtVerifier) discoverJWKS(ctx context.Context) (string, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	u := strings.TrimSuffix(v.issuer, "/") + "/.well-known/openid-configuration"
	if err := v.getJSON(ctx, u, &doc); err != nil {
		return "", fmt.Errorf("OpenID discovery: %w", err)
	}
	// Tokens are checked against the configured issuer verbatim, so catch a
	// mismatch (typically a missing trailing slash) here with a clear message
	// instead of rejecting every token later.
	if doc.Issuer != v.issuer {
		return "", fmt.Errorf("OpenID discovery at %s reports issuer %q, but AH_MCP_OAUTH_ISSUER is %q", u, doc.Issuer, v.issuer)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("OpenID discovery at %s has no jwks_uri", u)
	}
	return doc.JWKSURI, nil
}

func (v *jwtVerifier) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxOAuthDocBytes)).Decode(out)
}

// jwk is a parsed signing key from a JWKS.
type jwk struct {
	Kid string
	Alg string
	pub crypto.PublicKey
}

// parseJWK converts one JWKS entry into a public key. It returns nil, nil for
// keys that are not meant for signature verification.
func parseJWK(raw json.RawMessage) (*jwk, error) {
	var k struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, err
	}
	if k.Use != "" && k.Use != "sig" {
		return nil, nil
	}
	switch k.Kty {
	case "RSA":
		n, err := b64BigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("key %q: n: %w", k.Kid, err)
		}
		e, err := b64BigInt(k.E)
		if err != nil {
			return nil, fmt.Errorf("key %q: e: %w", k.Kid, err)
		}
		if n.BitLen() < minRSAKeyBits {
			return nil, fmt.Errorf("key %q: RSA modulus is %d bits, need at least %d", k.Kid, n.BitLen(), minRSAKeyBits)
		}
		if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, fmt.Errorf("key %q: unsupported RSA exponent", k.Kid)
		}
		return &jwk{Kid: k.Kid, Alg: k.Alg, pub: &rsa.PublicKey{N: n, E: int(e.Int64())}}, nil
	case "EC":
		pub, err := ecPublicKey(k.Crv, k.X, k.Y)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k.Kid, err)
		}
		return &jwk{Kid: k.Kid, Alg: k.Alg, pub: pub}, nil
	default:
		return nil, nil
	}
}

func ecPublicKey(crv, xs, ys string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	var checker ecdh.Curve
	switch crv {
	case "P-256":
		curve, checker = elliptic.P256(), ecdh.P256()
	case "P-384":
		curve, checker = elliptic.P384(), ecdh.P384()
	case "P-521":
		curve, checker = elliptic.P521(), ecdh.P521()
	default:
		return nil, fmt.Errorf("unsupported curve %q", crv)
	}
	x, err := b64BigInt(xs)
	if err != nil {
		return nil, fmt.Errorf("x: %w", err)
	}
	y, err := b64BigInt(ys)
	if err != nil {
		return nil, fmt.Errorf("y: %w", err)
	}
	// crypto/ecdh rejects points that are not on the curve, which guards
	// against invalid-curve attacks on the verifier.
	size := (curve.Params().BitSize + 7) / 8
	if len(x.Bytes()) > size || len(y.Bytes()) > size {
		return nil, errors.New("coordinate too large for curve")
	}
	point := make([]byte, 1+2*size)
	point[0] = 4
	x.FillBytes(point[1 : 1+size])
	y.FillBytes(point[1+size:])
	if _, err := checker.NewPublicKey(point); err != nil {
		return nil, fmt.Errorf("invalid EC point: %w", err)
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

func b64BigInt(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("missing")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}
