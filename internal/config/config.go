// Package config reads the workspace's optional configuration file (IDP_WORKSPACE_CONFIG): the
// downstream systems actions are forwarded to and the webhook signing secret (ADR-0001 D4).
// Everything else is environment.
package config

import (
	"errors"
	"fmt"
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
