package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	secret := write(t, "secret", strings.Repeat("s", 40))
	tok := write(t, "tok", "svc-token\n")
	f, err := Load(write(t, "c.yaml", `
targets:
  host: {endpoint: "https://host:8443"}
  eer: {endpoint: "https://eer:8080", auth: static, tokenFile: "`+tok+`", tenantId: "eer-tenant"}
webhooks: {secretFile: "`+secret+`"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Targets.Host.Auth != "forward" || f.Targets.EER.TenantID != "eer-tenant" {
		t.Fatalf("%+v", f)
	}
	if v, err := f.Targets.EER.Token(); err != nil || v != "svc-token" {
		t.Fatalf("%q %v", v, err)
	}
	for _, bad := range []string{
		`targets: {host: {endpoint: "ftp://x"}}`,
		`targets: {host: {endpoint: "https://x", auth: magic}}`,
		`targets: {host: {endpoint: "https://x", auth: static}}`,
		`targets: {eer: {endpoint: "https://x"}}`,
		`targets: {host: {endpoint: "https://x", auth: exchange}}`,
		`targets: {host: {endpoint: "https://x", auth: exchange, exchange: {tokenUrl: "https://idp/token", clientId: "c"}}}`,
		`webhooks: {secretFile: "` + write(t, "short", "abc") + `"}`,
		`targets: {cmi: {endpoint: "ftp://x"}}`,
		`targets: {cmi: {endpoint: "https://x", auth: magic}}`,
		`targets: {cmi: {endpoint: "https://x", auth: static}}`,
		`targets: {cmi: {endpoint: "https://x", auth: exchange}}`,
		`targets: {cmi: {endpoint: "https://x", auth: exchange, exchange: {tokenUrl: "https://idp/token", clientId: "c", clientSecretFile: "/s"}}}`,
		`targets: {cmi: {auth: forward}}`,
		`unknown: 1`,
	} {
		if _, err := Load(write(t, "bad.yaml", bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if f, err := Load(""); err != nil || f.Targets.Host != nil {
		t.Fatal(err)
	}
}

func TestLoadCMI(t *testing.T) {
	f, err := Load(write(t, "c.yaml", `
targets:
  cmi: {endpoint: "https://cmi:8443", auth: exchange, exchange: {tokenUrl: "https://idp/token", clientId: "idp-workspace", clientSecretFile: "/s", audience: "idp-cmi"}}
`))
	if err != nil || f.Targets.CMI == nil || f.Targets.CMI.Exchange.Audience != "idp-cmi" {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := Load(write(t, "d.yaml", `targets: {cmi: {endpoint: "https://cmi"}}`)); err != nil || f.Targets.CMI.Auth != "forward" {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := Load(write(t, "e.yaml", `targets: {host: {endpoint: "https://h"}}`)); err != nil || f.Targets.CMI != nil {
		t.Fatalf("%+v %v", f, err)
	}
}
