package sso

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"golang.org/x/sync/singleflight"

	"github.com/robertjndw/gotero/internal/config"
)

const (
	// MaxACSBody caps the SAMLResponse form. Real responses with encrypted
	// assertions are a few KiB.
	MaxACSBody = 1 << 20
	// metadataTTL is how long fetched IdP metadata is trusted before a
	// refetch, so IdP certificate rollovers get picked up.
	metadataTTL = 24 * time.Hour
	// metadataRetry is how long a stale copy is used after a failed refresh
	// before the next fetch is attempted.
	metadataRetry = 5 * time.Minute

	nameIDTransient = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
)

// Fallback email attribute names, tried after the configured one: the LDAP
// OID that most IdPs emit and the ADFS/Entra claim URI.
var fallbackEmailAttrs = []string{
	"urn:oid:0.9.2342.19200300.100.1.3",
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
}

// spKeyPair is gotero's own SAML key, shared by all SAML providers.
type spKeyPair struct {
	key  crypto.Signer
	cert *x509.Certificate
}

func loadServiceProvider(c config.SAMLServiceProvider) (*spKeyPair, error) {
	// tls.LoadX509KeyPair accepts PKCS1, PKCS8 and EC keys and checks that
	// the key matches the certificate.
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("sso: load saml sp key pair: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("sso: saml sp key cannot sign")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("sso: parse saml sp certificate: %w", err)
	}
	return &spKeyPair{key: signer, cert: cert}, nil
}

type samlProvider struct {
	cfg    config.SAMLProvider
	sp     saml.ServiceProvider // template; IDPMetadata is filled per call
	client *http.Client
	log    *slog.Logger

	// mu guards md and fetchedAt only; fetches run outside it, shared by
	// concurrent callers.
	mu        sync.Mutex
	md        *saml.EntityDescriptor
	fetchedAt time.Time
	fetch     singleflight.Group
}

func newSAML(publicURL string, c config.SAMLProvider, kp *spKeyPair, log *slog.Logger) (*samlProvider, error) {
	base := publicURL + "/auth/saml/" + c.Name
	metaURL, err := url.Parse(base + "/metadata")
	if err != nil {
		return nil, err
	}
	acsURL, err := url.Parse(base + "/acs")
	if err != nil {
		return nil, err
	}
	entityID := c.EntityID
	if entityID == "" {
		entityID = metaURL.String()
	}
	p := &samlProvider{
		cfg:    c,
		client: &http.Client{Timeout: httpTimeout},
		log:    log,
		sp: saml.ServiceProvider{
			EntityID:    entityID,
			Key:         kp.key,
			Certificate: kp.cert,
			MetadataURL: *metaURL,
			AcsURL:      *acsURL,
			// Persistent IDs are stable across logins; transient ones
			// can't identify a returning user.
			AuthnNameIDFormat: saml.PersistentNameIDFormat,
			AllowIDPInitiated: false,
			// crewjam treats a missing AudienceRestriction as valid; an
			// assertion usable at any SP is not something to accept.
			ValidateAudienceRestriction: func(a *saml.Assertion) error {
				if a.Conditions == nil {
					return errors.New("assertion has no conditions")
				}
				for _, ar := range a.Conditions.AudienceRestrictions {
					if ar.Audience.Value == entityID {
						return nil
					}
				}
				return fmt.Errorf("audience restriction does not contain %q", entityID)
			},
		},
	}
	if c.IDPMetadataFile != "" {
		raw, err := os.ReadFile(c.IDPMetadataFile)
		if err != nil {
			return nil, fmt.Errorf("read idp metadata: %w", err)
		}
		md, err := samlsp.ParseMetadata(raw)
		if err != nil {
			return nil, fmt.Errorf("parse idp metadata: %w", err)
		}
		p.md = md
	} else if c.IDPMetadataURL == "" {
		return nil, errors.New("no idp metadata configured")
	}
	return p, nil
}

func (p *samlProvider) Name() string        { return p.cfg.Name }
func (p *samlProvider) Kind() Kind          { return KindSAML }
func (p *samlProvider) DisplayName() string { return p.cfg.DisplayName }
func (p *samlProvider) AutoProvision() bool { return p.cfg.AutoProvision }
func (p *samlProvider) LinkByEmail() bool   { return p.cfg.LinkByEmail }

// idpMetadata returns file metadata as is, and URL metadata cached for
// metadataTTL. A failed refresh keeps serving the stale copy; a failed first
// fetch is retried on the next call.
func (p *samlProvider) idpMetadata(ctx context.Context) (*saml.EntityDescriptor, error) {
	if p.cfg.IDPMetadataFile != "" {
		return p.md, nil
	}
	p.mu.Lock()
	cached, fresh := p.md, time.Since(p.fetchedAt) < metadataTTL
	p.mu.Unlock()
	if cached != nil && fresh {
		return cached, nil
	}
	v, err, _ := p.fetch.Do("", func() (any, error) {
		u, err := url.Parse(p.cfg.IDPMetadataURL)
		if err != nil {
			return nil, err
		}
		// Detached so one canceled request does not fail the others sharing
		// this fetch; the client timeout still bounds it.
		md, err := samlsp.FetchMetadata(context.WithoutCancel(ctx), p.client, *u)
		if err != nil {
			p.mu.Lock()
			if p.md != nil {
				// Keep serving the stale copy for a while instead of making
				// every login wait out the client timeout again.
				p.fetchedAt = time.Now().Add(metadataRetry - metadataTTL)
			}
			p.mu.Unlock()
			return nil, err
		}
		p.mu.Lock()
		p.md, p.fetchedAt = md, time.Now()
		p.mu.Unlock()
		return md, nil
	})
	if err != nil {
		if cached != nil {
			p.log.Warn("saml idp metadata refresh failed, using cached copy", "provider", p.cfg.Name, "err", err)
			return cached, nil
		}
		return nil, fmt.Errorf("saml %s: fetch idp metadata: %w", p.cfg.Name, err)
	}
	return v.(*saml.EntityDescriptor), nil
}

// serviceProvider returns a copy so a metadata refresh never mutates a
// ServiceProvider another request is validating against.
func (p *samlProvider) serviceProvider(ctx context.Context) (*saml.ServiceProvider, error) {
	md, err := p.idpMetadata(ctx)
	if err != nil {
		return nil, err
	}
	sp := p.sp
	sp.IDPMetadata = md
	return &sp, nil
}

func (p *samlProvider) AuthnRequest(ctx context.Context, relayState string) (string, string, error) {
	sp, err := p.serviceProvider(ctx)
	if err != nil {
		return "", "", err
	}
	req, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", "", fmt.Errorf("saml %s: build authn request: %w", p.cfg.Name, err)
	}
	u, err := req.Redirect(relayState, sp)
	if err != nil {
		return "", "", fmt.Errorf("saml %s: encode authn request: %w", p.cfg.Name, err)
	}
	return u.String(), req.ID, nil
}

func (p *samlProvider) ParseResponse(r *http.Request, requestID string) (Identity, error) {
	if requestID == "" {
		return Identity{}, errors.New("saml: no request id")
	}
	if r.PostForm == nil {
		r.Body = http.MaxBytesReader(nil, r.Body, MaxACSBody)
		if err := r.ParseForm(); err != nil {
			return Identity{}, fmt.Errorf("saml %s: read form: %w", p.cfg.Name, err)
		}
	}
	// crewjam would resolve an artifact by calling the IdP, and r.Form also
	// includes query parameters; only the POST binding is supported.
	if r.Form.Get("SAMLart") != "" || r.PostForm.Get("SAMLResponse") == "" {
		return Identity{}, fmt.Errorf("saml %s: no SAMLResponse in POST body", p.cfg.Name)
	}
	sp, err := p.serviceProvider(r.Context())
	if err != nil {
		return Identity{}, err
	}
	a, err := sp.ParseResponse(r, []string{requestID})
	if err != nil {
		// crewjam hides the cause behind a static message for the benefit
		// of HTTP responses; the cause is what the server log needs.
		var ive *saml.InvalidResponseError
		if errors.As(err, &ive) && ive.PrivateErr != nil {
			return Identity{}, fmt.Errorf("saml %s: invalid response: %w", p.cfg.Name, ive.PrivateErr)
		}
		return Identity{}, fmt.Errorf("saml %s: invalid response: %w", p.cfg.Name, err)
	}
	return p.identity(a)
}

func (p *samlProvider) identity(a *saml.Assertion) (Identity, error) {
	var id Identity
	if p.cfg.SubjectAttribute != "" {
		id.Subject = attr(a, p.cfg.SubjectAttribute)
	} else {
		if a.Subject == nil || a.Subject.NameID == nil {
			return id, fmt.Errorf("saml %s: assertion has no NameID", p.cfg.Name)
		}
		// A transient NameID changes on every login, so using it as the
		// subject would create a new account each time.
		if a.Subject.NameID.Format == nameIDTransient {
			return id, fmt.Errorf("saml %s: transient NameID cannot identify a user; set a subject attribute", p.cfg.Name)
		}
		id.Subject = strings.TrimSpace(a.Subject.NameID.Value)
	}
	if id.Subject == "" {
		return id, fmt.Errorf("saml %s: assertion has no subject", p.cfg.Name)
	}
	id.Email = attr(a, append([]string{p.cfg.EmailAttribute}, fallbackEmailAttrs...)...)
	// SAML has no email_verified. Whether this IdP's emails may be used for
	// linking is the operator's call (LINK_BY_EMAIL), enforced by the caller.
	id.EmailVerified = id.Email != ""
	id.Username = attr(a, p.cfg.UsernameAttribute)
	id.DisplayName = attr(a, p.cfg.NameAttribute)
	return id, nil
}

// attr returns the first non-empty value of the first attribute matching
// one of names, by Name or FriendlyName.
func attr(a *saml.Assertion, names ...string) string {
	for _, want := range names {
		if want == "" {
			continue
		}
		for _, st := range a.AttributeStatements {
			for _, at := range st.Attributes {
				if at.Name != want && at.FriendlyName != want {
					continue
				}
				for _, v := range at.Values {
					if s := strings.TrimSpace(v.Value); s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

func (p *samlProvider) Metadata(context.Context) ([]byte, error) {
	md := p.sp.Metadata()
	// crewjam also advertises the artifact binding; we only handle POST.
	for i := range md.SPSSODescriptors {
		var post []saml.IndexedEndpoint
		for _, e := range md.SPSSODescriptors[i].AssertionConsumerServices {
			if e.Binding == saml.HTTPPostBinding {
				post = append(post, e)
			}
		}
		md.SPSSODescriptors[i].AssertionConsumerServices = post
	}
	out, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}
