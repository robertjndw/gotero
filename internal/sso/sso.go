// Package sso implements the protocol side of OIDC and SAML sign-in. It
// turns an IdP response into an Identity and nothing more: matching that
// identity to a gotero account, and every decision about roles, lives in the
// callers. No group or role claim from an IdP is ever read here.
package sso

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/robertjndw/gotero/internal/config"
)

type Kind string

const (
	KindOIDC Kind = "oidc"
	KindSAML Kind = "saml"
)

// httpTimeout bounds every call to an IdP so a hung provider can't pin
// a login request open.
const httpTimeout = 10 * time.Second

// Identity is what an IdP vouched for. Subject is the only stable key;
// everything else is advisory profile data.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Username      string
	DisplayName   string
}

type Provider interface {
	Name() string
	Kind() Kind
	DisplayName() string
	AutoProvision() bool
	LinkByEmail() bool
}

type OIDC interface {
	Provider
	AuthURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error)
	Exchange(ctx context.Context, code, nonce, pkceVerifier string) (Identity, error)
}

type SAML interface {
	Provider
	// AuthnRequest returns the IdP redirect URL (HTTP-Redirect binding) and
	// the request ID the caller must store and pass to ParseResponse.
	AuthnRequest(ctx context.Context, relayState string) (redirectURL, requestID string, err error)
	// ParseResponse validates an HTTP-POST binding response; r's form must
	// contain SAMLResponse.
	ParseResponse(r *http.Request, requestID string) (Identity, error)
	Metadata(ctx context.Context) ([]byte, error)
}

// Registry holds the configured providers. A nil Registry is valid and empty.
type Registry struct {
	list   []Provider
	byName map[string]Provider
}

// New builds providers from cfg. It never contacts an OIDC issuer or a SAML
// metadata URL, so a down IdP can't keep gotero from starting.
func New(cfg *config.Config, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{byName: map[string]Provider{}}
	add := func(p Provider) error {
		if _, dup := r.byName[p.Name()]; dup {
			return fmt.Errorf("sso: duplicate provider name %q", p.Name())
		}
		r.byName[p.Name()] = p
		r.list = append(r.list, p)
		return nil
	}
	for _, pc := range cfg.OIDC {
		if err := add(newOIDC(cfg.PublicURL, pc, log)); err != nil {
			return nil, err
		}
	}
	if len(cfg.SAML) > 0 {
		sp, err := loadServiceProvider(cfg.SAMLSP)
		if err != nil {
			return nil, err
		}
		for _, pc := range cfg.SAML {
			p, err := newSAML(cfg.PublicURL, pc, sp, log)
			if err != nil {
				return nil, fmt.Errorf("sso: saml provider %q: %w", pc.Name, err)
			}
			if err := add(p); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

func (r *Registry) Get(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byName[name]
	return p, ok
}

// List returns providers in config order, OIDC first, then SAML.
func (r *Registry) List() []Provider {
	if r == nil {
		return nil
	}
	return append([]Provider(nil), r.list...)
}

func NewPKCEVerifier() string { return oauth2.GenerateVerifier() }
