package apptoken

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestIssueAndVerifyWithTheJWKS(t *testing.T) {
	s, err := Generate("https://workspace.example")
	if err != nil {
		t.Fatal(err)
	}
	tok, exp, err := s.Issue(Claims{Subject: "bob", Tenant: "t1", WorkspaceID: "w1", Workspace: "us-ops", App: "orders", AppVersion: "1.0.0", Roles: []string{"approver"}})
	if err != nil || time.Until(exp) > TTL || time.Until(exp) < TTL-time.Minute {
		t.Fatal(err, exp)
	}
	// verify with the key reconstructed from the published JWKS only
	var set struct {
		Keys []struct{ Kid, X, Y, Crv, Alg string }
	}
	if err := json.Unmarshal(s.JWKS(), &set); err != nil || len(set.Keys) != 1 || set.Keys[0].Alg != "ES256" {
		t.Fatalf("%s", s.JWKS())
	}
	x, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].X)
	y, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].Y)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		if t.Header["kid"] != set.Keys[0].Kid {
			t.Header["kid"] = "mismatch"
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer("https://workspace.example"), jwt.WithAudience("idp-workspace-app:orders"))
	if err != nil || !parsed.Valid || parsed.Header["kid"] != set.Keys[0].Kid {
		t.Fatal(err)
	}
	if claims["sub"] != "bob" || claims["tenant_id"] != "t1" || claims["workspace"].(map[string]any)["name"] != "us-ops" || claims["appVersion"] != "1.0.0" {
		t.Fatalf("%v", claims)
	}
	// another app's audience does not accept it
	if _, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return pub, nil }, jwt.WithAudience("idp-workspace-app:billing")); err == nil {
		t.Fatal("a token for orders verified for billing")
	}
}

func TestLoad(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	p := filepath.Join(dir, "k.pem")
	_ = os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	s, err := Load("https://w.example", p)
	if err != nil || !s.Public().Equal(&key.PublicKey) {
		t.Fatal(err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ = x509.MarshalECPrivateKey(p384)
	_ = os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
	if _, err := Load("https://w.example", p); err == nil {
		t.Fatal("accepted a P-384 key")
	}
	if _, err := Generate(""); err == nil {
		t.Fatal("accepted no issuer")
	}
}
