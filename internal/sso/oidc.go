package sso

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/robertjndw/gotero/internal/config"
)

type oidcProvider struct {
	cfg    config.OIDCProvider
	oauth  oauth2.Config
	client *http.Client
	log    *slog.Logger

	// Discovery happens on first use and is retried after a failure, so the
	// IdP being down at startup only breaks its own login button.
	mu       sync.Mutex
	provider *oidc.Provider
}

func newOIDC(publicURL string, c config.OIDCProvider, log *slog.Logger) *oidcProvider {
	p := &oidcProvider{
		cfg:    c,
		client: &http.Client{Timeout: httpTimeout},
		log:    log,
		oauth: oauth2.Config{
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			RedirectURL:  publicURL + "/auth/oidc/" + c.Name + "/callback",
			Scopes:       c.Scopes,
		},
	}
	return p
}

func (p *oidcProvider) Name() string        { return p.cfg.Name }
func (p *oidcProvider) Kind() Kind          { return KindOIDC }
func (p *oidcProvider) DisplayName() string { return p.cfg.DisplayName }
func (p *oidcProvider) AutoProvision() bool { return p.cfg.AutoProvision }
func (p *oidcProvider) LinkByEmail() bool   { return p.cfg.LinkByEmail }

func (p *oidcProvider) ctx(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, p.client)
}

// discover returns the cached provider, running discovery if needed.
func (p *oidcProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	prov, err := oidc.NewProvider(p.ctx(ctx), p.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc %s: discovery: %w", p.cfg.Name, err)
	}
	p.provider = prov
	return prov, nil
}

// config returns the oauth2 config with the discovered endpoint.
func (p *oidcProvider) config(prov *oidc.Provider) *oauth2.Config {
	c := p.oauth
	c.Endpoint = prov.Endpoint()
	if c.ClientSecret == "" {
		// Public client: never send an empty secret in a Basic header.
		c.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	return &c
}

func (p *oidcProvider) AuthURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return p.config(prov).AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier),
	), nil
}

func (p *oidcProvider) Exchange(ctx context.Context, code, nonce, pkceVerifier string) (Identity, error) {
	if nonce == "" || pkceVerifier == "" {
		return Identity{}, errors.New("oidc: missing nonce or pkce verifier")
	}
	prov, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	ctx = p.ctx(ctx)
	tok, err := p.config(prov).Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc %s: token exchange: %w", p.cfg.Name, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, fmt.Errorf("oidc %s: token response has no id_token", p.cfg.Name)
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc %s: verify id_token: %w", p.cfg.Name, err)
	}
	if idt.Nonce != nonce {
		return Identity{}, fmt.Errorf("oidc %s: nonce mismatch", p.cfg.Name)
	}
	if idt.Subject == "" {
		return Identity{}, fmt.Errorf("oidc %s: id_token has no sub", p.cfg.Name)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("oidc %s: decode claims: %w", p.cfg.Name, err)
	}

	id := Identity{Subject: idt.Subject}
	id.Email, _ = claims["email"].(string)
	id.EmailVerified = claimBool(claims["email_verified"])

	if id.Email == "" {
		// Some IdPs keep email out of the ID token. UserInfo is only
		// trusted if it describes the same subject.
		info, err := prov.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			p.log.Warn("oidc userinfo failed", "provider", p.cfg.Name, "err", err)
		} else if info.Subject == idt.Subject {
			var uc map[string]any
			if err := info.Claims(&uc); err == nil {
				id.Email, _ = uc["email"].(string)
				id.EmailVerified = claimBool(uc["email_verified"])
				if _, ok := claims[p.cfg.UsernameClaim]; !ok {
					claims[p.cfg.UsernameClaim] = uc[p.cfg.UsernameClaim]
				}
				if _, ok := claims["name"]; !ok {
					claims["name"] = uc["name"]
				}
			}
		}
	}
	if id.Email != "" && p.cfg.TrustEmail {
		id.EmailVerified = true
	}
	if id.Email == "" {
		id.EmailVerified = false
	}
	id.Username, _ = claims[p.cfg.UsernameClaim].(string)
	id.DisplayName, _ = claims["name"].(string)
	return id, nil
}

// claimBool accepts a JSON bool or the string "true"; some IdPs
// (Cognito, for one) send the string form.
func claimBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}
