package ssotest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	samlIdPEntityID = "https://idp.example.test/metadata"
	samlIdPSSOURL   = "https://idp.example.test/sso"
)

// SAMLUser is the person the fake IdP authenticates.
type SAMLUser struct {
	NameID string
	// NameIDFormat defaults to persistent. Use the transient URN to test
	// rejection.
	NameIDFormat string
	Email        string
	Username     string
	DisplayName  string
	// Attributes are extra attributes by Name, e.g. a custom subject attribute.
	Attributes map[string]string
}

// SAMLOptions bends an otherwise valid response so tests can exercise the
// SP's checks. The zero value produces a valid response.
type SAMLOptions struct {
	// Now is the IdP's clock. A time in the past yields an expired response.
	Now time.Time
	// InResponseTo replaces the ID of the request being answered.
	InResponseTo string
	// Unsolicited drops InResponseTo entirely, like an IdP-initiated login.
	Unsolicited bool
	// Audience replaces the audience restriction.
	Audience string
}

// SAMLPost is what the browser would POST to the ACS URL.
type SAMLPost struct {
	SAMLResponse string // base64
	RelayState   string
	ACSURL       string
}

// Request builds the POST request to the ACS.
func (p SAMLPost) Request() *http.Request {
	form := url.Values{"SAMLResponse": {p.SAMLResponse}, "RelayState": {p.RelayState}}
	u, _ := url.Parse(p.ACSURL)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, u.Path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// TamperSignature flips a character in the XML signature value, leaving the
// signed content alone.
func (p SAMLPost) TamperSignature(t testing.TB) SAMLPost {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(p.SAMLResponse)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	const open = "SignatureValue>"
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatal("response has no SignatureValue")
	}
	i += len(open)
	for s[i] == ' ' || s[i] == '\n' {
		i++
	}
	flipped := byte('A')
	if s[i] == 'A' {
		flipped = 'B'
	}
	s = s[:i] + string(flipped) + s[i+1:]
	p.SAMLResponse = base64.StdEncoding.EncodeToString([]byte(s))
	return p
}

// Replace edits the signed response XML, invalidating its signature.
func (p SAMLPost) Replace(t testing.TB, old, new string) SAMLPost {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(p.SAMLResponse)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("response does not contain %q", old)
	}
	p.SAMLResponse = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(raw), old, new, 1)))
	return p
}

// SAMLIdP is an in-process SAML identity provider. It has no listener: tests
// hand it the redirect URL from sso's AuthnRequest and get back the POST the
// browser would send to the ACS.
type SAMLIdP struct {
	idp *saml.IdentityProvider
	sp  *saml.EntityDescriptor
}

func NewSAMLIdP(t testing.TB) *SAMLIdP {
	t.Helper()
	key, cert := selfSigned(t, "idp.example.test")
	ssoURL, _ := url.Parse(samlIdPSSOURL)
	metaURL, _ := url.Parse(samlIdPEntityID)
	i := &SAMLIdP{}
	i.idp = &saml.IdentityProvider{
		Key:                     key,
		Signer:                  key,
		Certificate:             cert,
		MetadataURL:             *metaURL,
		SSOURL:                  *ssoURL,
		SignatureMethod:         dsig.RSASHA256SignatureMethod,
		ServiceProviderProvider: spProvider{func() *saml.EntityDescriptor { return i.sp }},
	}
	return i
}

type spProvider struct{ get func() *saml.EntityDescriptor }

func (p spProvider) GetServiceProvider(_ *http.Request, _ string) (*saml.EntityDescriptor, error) {
	if sp := p.get(); sp != nil {
		return sp, nil
	}
	return nil, os.ErrNotExist
}

// TrustSP registers the SP metadata (from sso's SAML.Metadata) the IdP will
// answer. It is separate from NewSAMLIdP because the SP needs the IdP
// metadata file first.
func (i *SAMLIdP) TrustSP(t testing.TB, spMetadata []byte) {
	t.Helper()
	var ed saml.EntityDescriptor
	if err := xml.Unmarshal(spMetadata, &ed); err != nil {
		t.Fatal(err)
	}
	i.sp = &ed
}

// EntityID is the IdP's issuer.
func (i *SAMLIdP) EntityID() string { return samlIdPEntityID }

// WriteMetadataFile writes the IdP metadata XML into t.TempDir().
func (i *SAMLIdP) WriteMetadataFile(t testing.TB) string {
	t.Helper()
	out, err := xml.MarshalIndent(i.idp.Metadata(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "idp-metadata.xml")
	if err := os.WriteFile(path, append([]byte(xml.Header), out...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Respond answers the AuthnRequest in redirectURL with a valid signed
// response for user.
func (i *SAMLIdP) Respond(t testing.TB, redirectURL string, user SAMLUser) SAMLPost {
	return i.RespondWith(t, redirectURL, user, SAMLOptions{})
}

// RespondWith is Respond with deliberate defects from opts.
func (i *SAMLIdP) RespondWith(t testing.TB, redirectURL string, user SAMLUser, opts SAMLOptions) SAMLPost {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, redirectURL, nil)
	req, err := saml.NewIdpAuthnRequest(i.idp, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if !opts.Now.IsZero() {
		req.Now = opts.Now
	}
	if opts.Unsolicited {
		req.Request.ID = ""
	} else if opts.InResponseTo != "" {
		req.Request.ID = opts.InResponseTo
	}

	format := user.NameIDFormat
	if format == "" {
		format = string(saml.PersistentNameIDFormat)
	}
	sess := &saml.Session{
		ID:           "session-1",
		CreateTime:   req.Now,
		ExpireTime:   req.Now.Add(time.Hour),
		Index:        "1",
		NameID:       user.NameID,
		NameIDFormat: format,
		UserEmail:    user.Email,
		UserName:     user.Username,
	}
	if user.DisplayName != "" {
		sess.CustomAttributes = append(sess.CustomAttributes, attribute("displayName", user.DisplayName))
	}
	if user.Email != "" {
		sess.CustomAttributes = append(sess.CustomAttributes, attribute("email", user.Email))
	}
	if user.Username != "" {
		sess.CustomAttributes = append(sess.CustomAttributes, attribute("uid", user.Username))
	}
	for k, v := range user.Attributes {
		sess.CustomAttributes = append(sess.CustomAttributes, attribute(k, v))
	}
	if err := (saml.DefaultAssertionMaker{}).MakeAssertion(req, sess); err != nil {
		t.Fatal(err)
	}
	if opts.Audience != "" {
		req.Assertion.Conditions.AudienceRestrictions[0].Audience.Value = opts.Audience
	}
	form, err := req.PostBinding()
	if err != nil {
		t.Fatal(err)
	}
	return SAMLPost{SAMLResponse: form.SAMLResponse, RelayState: form.RelayState, ACSURL: form.URL}
}

func attribute(name, value string) saml.Attribute {
	return saml.Attribute{
		Name:       name,
		NameFormat: "urn:oasis:names:tc:SAML:2.0:attrname-format:basic",
		Values:     []saml.AttributeValue{{Type: "xs:string", Value: value}},
	}
}

// WriteSPKeyPair writes a self-signed certificate and key for the SP into
// t.TempDir(). pkcs8 selects the key encoding (otherwise PKCS1).
func WriteSPKeyPair(t testing.TB, pkcs8 bool) (certFile, keyFile string) {
	t.Helper()
	key, cert := selfSigned(t, "sp.example.test")
	var keyPEM []byte
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	} else {
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "sp.crt"), filepath.Join(dir, "sp.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func selfSigned(t testing.TB, cn string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}
