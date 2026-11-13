// Package apptoken issues the short-lived tokens an app's backend receives instead of the user's
// credentials (ADR-0001 D15): signed by the workspace (ES256), audience idp-workspace-app:<app>,
// naming the member, the tenant, the workspace and the member's workspace roles. The public keys are
// served as a JWKS so app backends verify without sharing a secret.
package apptoken

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TTL is how long an app token is valid.
const TTL = 5 * time.Minute

// AudiencePrefix + app name is the audience of an app's tokens.
const AudiencePrefix = "idp-workspace-app:"

// Signer holds the workspace's signing key.
type Signer struct {
	Issuer string
	key    *ecdsa.PrivateKey
	kid    string
	Now    func() time.Time
}

// New wraps a P-256 key.
func New(issuer string, key *ecdsa.PrivateKey) (*Signer, error) {
	if key.Curve != elliptic.P256() {
		return nil, errors.New("the signing key must be a P-256 EC key")
	}
	if issuer == "" {
		return nil, errors.New("the issuer (IDP_WORKSPACE_PUBLIC_URL) is required")
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(der)
	return &Signer{Issuer: issuer, key: key, kid: base64.RawURLEncoding.EncodeToString(sum[:12])}, nil
}

// Generate makes an ephemeral key (development: tokens stop verifying after a restart).
func Generate(issuer string) (*Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return New(issuer, key)
}

// Load reads a PEM key ("EC PRIVATE KEY" or PKCS#8 "PRIVATE KEY").
func Load(issuer, path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("signing key: no PEM block")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var k any
		if k, err = x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			var ok bool
			if key, ok = k.(*ecdsa.PrivateKey); !ok {
				err = errors.New("signing key: not an EC key")
			}
		}
	default:
		err = fmt.Errorf("signing key: unexpected PEM type %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	return New(issuer, key)
}

// Claims of an app token.
type Claims struct {
	Subject     string
	Tenant      string
	WorkspaceID string
	Workspace   string
	App         string
	AppVersion  string
	Roles       []string
}

// Issue signs a token for claims; it returns the token and its expiry.
func (s *Signer) Issue(c Claims) (string, time.Time, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	exp := now.Add(TTL)
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	roles := c.Roles
	if roles == nil {
		roles = []string{}
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": s.Issuer, "aud": AudiencePrefix + c.App, "sub": c.Subject, "tenant_id": c.Tenant,
		"workspace": map[string]string{"id": c.WorkspaceID, "name": c.Workspace}, "app": c.App, "appVersion": c.AppVersion,
		"roles": roles, "iat": now.Unix(), "nbf": now.Unix(), "exp": exp.Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti),
	})
	t.Header["kid"] = s.kid
	signed, err := t.SignedString(s.key)
	return signed, exp, err
}

// JWKS is the public key set.
func (s *Signer) JWKS() []byte {
	pad := func(b []byte) string {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	raw, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "kid": s.kid,
		"x": pad(s.key.PublicKey.X.Bytes()), "y": pad(s.key.PublicKey.Y.Bytes()),
	}}})
	return raw
}

// Public returns the verification key (tests, Go app backends).
func (s *Signer) Public() *ecdsa.PublicKey { return &s.key.PublicKey }

// Sign signs other claims with the workspace's key (file tickets, ADR-0001 D23); the caller sets iss, aud and exp.
func (s *Signer) Sign(claims jwt.MapClaims) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = s.kid
	return t.SignedString(s.key)
}

// Verify checks a token this signer issued for audience: ES256 with this key, issuer, audience, expiry.
func (s *Signer) Verify(token, audience string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(s.Issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithLeeway(5 * time.Second)}
	if s.Now != nil {
		opts = append(opts, jwt.WithTimeFunc(s.Now))
	}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, opts...); err != nil {
		return nil, err
	}
	return claims, nil
}
