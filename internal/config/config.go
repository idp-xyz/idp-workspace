// Package config reads the workspace's optional configuration file (IDP_WORKSPACE_CONFIG): the
// downstream systems actions are forwarded to and the webhook signing secret (ADR-0001 D4).
// Everything else is environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the configuration file.
type File struct {
	Targets  Targets   `yaml:"targets"`
	Webhooks *Webhooks `yaml:"webhooks"`
}

// Targets are the systems that own decisions.
type Targets struct {
	Host *Downstream `yaml:"host"`
	EER  *EER        `yaml:"eer"`
	// CMI is idp-cmi, read and revoked as the member for "my memory" (ADR-0001 D21); absent = the
	// memory endpoints answer 404 NOT_CONFIGURED.
	CMI *Downstream `yaml:"cmi"`
	// Connections is the connected services (ADR-0001 D30): the internal API of idp-mcp-bridge, called with the member's
	// token exchanged for the bridge's audience; absent = the feature is off (no page, no routes).
	Connections *Connections `yaml:"connections"`
}

// Connections is the bridge's internal API and what the Workspace needs to host the OAuth callback and the client
// metadata documents of its connections.
type Connections struct {
	Downstream `yaml:",inline"`
	// PublicURL is the Workspace's public base URL (scheme and host, no path): the callback is PublicURL + /oauth/callback and
	// a connection's client metadata document PublicURL + /oauth/client/<id>.json.
	PublicURL string `yaml:"publicUrl"`
	// TestOrigins are exact host:port pairs of acceptance origins that may be plain http (the authorization address of a test
	// third party, the public URL of a test deployment). Never set in a deployment; it mirrors the bridge's IDP_BRIDGE_TEST_ORIGINS.
	TestOrigins []string `yaml:"testOrigins"`
}

// Downstream is one system's endpoint and how the workspace authenticates to it.
type Downstream struct {
	Endpoint  string        `yaml:"endpoint"`
	CAFile    string        `yaml:"caFile"`
	Auth      string        `yaml:"auth"`      // forward (default) | static | exchange
	TokenFile string        `yaml:"tokenFile"` // static: the service token, re-read on every call (rotation)
	Exchange  *Exchange     `yaml:"exchange"`  // exchange: RFC 8693 at the identity provider (ADR-0001 D16)
	Timeout   time.Duration `yaml:"timeout"`
}

// Exchange is the identity provider's token endpoint and the workspace's client there.
type Exchange struct {
	TokenURL         string `yaml:"tokenUrl"`
	ClientID         string `yaml:"clientId"`
	ClientSecretFile string `yaml:"clientSecretFile"`
	Audience         string `yaml:"audience"`
	CAFile           string `yaml:"caFile"`
	// SessionBound asks for a refresh token in the member's own session and discards it (ADR-0001 D25): Keycloak then
	// lets the downstream (idp-host) exchange this token again for a refresh token of its own, so that a long run keeps
	// working until the member signs out instead of dying with the member's access token.
	SessionBound bool `yaml:"sessionBound"`
}

// Secret reads the client secret file (re-read on every exchange: rotation).
func (x *Exchange) Secret() (string, error) {
	b, err := os.ReadFile(x.ClientSecretFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// EER adds the tenant id idp-eer expects in its request bodies.
type EER struct {
	Downstream `yaml:",inline"`
	TenantID   string `yaml:"tenantId"`
}

// Webhooks configures signed calls to apps' backends.
type Webhooks struct {
	SecretFile string        `yaml:"secretFile"`
	Timeout    time.Duration `yaml:"timeout"`
	CAFile     string        `yaml:"caFile"`
}

// Load reads and validates the file; path "" means no file (no targets).
func Load(path string) (*File, error) {
	f := &File{}
	if path == "" {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if d := f.Targets.Host; d != nil {
		if err := d.check("targets.host"); err != nil {
			return nil, err
		}
	}
	if d := f.Targets.CMI; d != nil {
		if err := d.check("targets.cmi"); err != nil {
			return nil, err
		}
	}
	if c := f.Targets.Connections; c != nil {
		if err := c.check(); err != nil {
			return nil, err
		}
	}
	if e := f.Targets.EER; e != nil {
		if err := e.check("targets.eer"); err != nil {
			return nil, err
		}
		if e.TenantID == "" {
			return nil, errors.New("targets.eer: tenantId (idp-eer's tenant) is required")
		}
	}
	if w := f.Webhooks; w != nil {
		if w.SecretFile == "" {
			return nil, errors.New("webhooks: secretFile is required")
		}
		b, err := os.ReadFile(w.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("webhooks: %w", err)
		}
		if len(strings.TrimSpace(string(b))) < 32 {
			return nil, errors.New("webhooks: the secret must be at least 32 bytes")
		}
	}
	return f, nil
}

func (c *Connections) check() error {
	const name = "targets.connections"
	if err := c.Downstream.check(name); err != nil {
		return err
	}
	// the bridge takes the member from an exchanged token and from nothing else (its D16): forwarding the member's own token
	// or sending a service credential would be refused there, and a service credential must never name a member
	if c.Auth != "exchange" {
		return fmt.Errorf("%s: auth must be exchange (the member's token exchanged for the bridge's audience)", name)
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: publicUrl must be the Workspace's public address, scheme and host only (https://workspace.example)", name)
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	for _, o := range c.TestOrigins {
		if h, port, err := net.SplitHostPort(o); err != nil || h == "" || port == "" {
			return fmt.Errorf("%s: testOrigins entries are host:port pairs, not %q", name, o)
		}
	}
	// plain http only for an origin the test settings name (the endpoint of the bridge inside the cluster is a separate matter: it is internal)
	if u.Scheme == "http" && !contains(c.TestOrigins, u.Host) {
		return fmt.Errorf("%s: publicUrl must be https (an http address only for an origin listed in testOrigins)", name)
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (d *Downstream) check(name string) error {
	if !strings.HasPrefix(d.Endpoint, "http://") && !strings.HasPrefix(d.Endpoint, "https://") {
		return fmt.Errorf("%s: endpoint (http(s)) is required", name)
	}
	switch d.Auth {
	case "":
		d.Auth = "forward"
	case "forward":
	case "static":
		if d.TokenFile == "" {
			return fmt.Errorf("%s: static auth needs tokenFile", name)
		}
	case "exchange":
		x := d.Exchange
		if x == nil || (!strings.HasPrefix(x.TokenURL, "http://") && !strings.HasPrefix(x.TokenURL, "https://")) || x.ClientID == "" || x.ClientSecretFile == "" || x.Audience == "" {
			return fmt.Errorf("%s: exchange auth needs exchange.tokenUrl, clientId, clientSecretFile and audience", name)
		}
	default:
		return fmt.Errorf("%s: auth must be forward, static or exchange", name)
	}
	return nil
}

// Token reads the static token file.
func (d *Downstream) Token() (string, error) {
	b, err := os.ReadFile(d.TokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
