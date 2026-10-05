// Package samlidp is a minimal in-process SAML 2.0 identity provider for the
// conformance suite's SSO leg: HTTP-Redirect binding in, HTTP-POST binding
// out, one signed assertion per sign-in. It signs with a key it generates at
// start, and the suite registers its certificate inline through the admin
// identity-provider API, so the deployment never calls it: only the browser
// does. That is also why it may listen on loopback over plain HTTP, which the
// provider validation admits for loopback hosts only.
package samlidp

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	protocolNS  = "urn:oasis:names:tc:SAML:2.0:protocol"
	assertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
)

// IdP is one running identity provider.
type IdP struct {
	EntityID string
	SSOURL   string
	// CertificatePEM is the public signing certificate to register.
	CertificatePEM string

	server   *http.Server
	listener net.Listener
	cert     tls.Certificate

	mu       sync.Mutex
	subject  string
	audience string
	// Requests counts the authentication requests answered.
	requests int
}

// Start listens on a loopback port and serves /sso.
func Start() (*IdP, error) {
	cert, certPEM, err := selfSigned()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	base := "http://" + listener.Addr().String()
	idp := &IdP{
		EntityID:       base + "/metadata",
		SSOURL:         base + "/sso",
		CertificatePEM: certPEM,
		listener:       listener,
		cert:           cert,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sso", idp.sso)
	idp.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = idp.server.Serve(listener) }()
	return idp, nil
}

// Close stops the server.
func (idp *IdP) Close() error { return idp.server.Close() }

// SignInAs sets the person the next assertions name (NameID and the `email`
// attribute). audience overrides the AudienceRestriction; "" names the
// requesting service provider, as a correct identity provider does.
func (idp *IdP) SignInAs(email, audience string) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.subject, idp.audience = email, audience
}

// Requests reports how many authentication requests the IdP answered.
func (idp *IdP) Requests() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.requests
}

// authnRequest is what the service provider asked for.
type authnRequest struct {
	ID     string
	Issuer string
	ACSURL string
}

func parseRedirectRequest(encoded string) (authnRequest, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return authnRequest{}, fmt.Errorf("SAMLRequest is not base64: %w", err)
	}
	inflated, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), 1<<20))
	if err != nil {
		return authnRequest{}, fmt.Errorf("SAMLRequest does not inflate: %w", err)
	}
	document := etree.NewDocument()
	if err := document.ReadFromBytes(inflated); err != nil {
		return authnRequest{}, fmt.Errorf("SAMLRequest is not XML: %w", err)
	}
	root := document.Root()
	if root == nil || root.Tag != "AuthnRequest" {
		return authnRequest{}, errors.New("SAMLRequest is not an AuthnRequest")
	}
	request := authnRequest{
		ID:     root.SelectAttrValue("ID", ""),
		ACSURL: root.SelectAttrValue("AssertionConsumerServiceURL", ""),
	}
	if issuer := root.SelectElement("Issuer"); issuer != nil {
		request.Issuer = issuer.Text()
	}
	if request.ID == "" || request.ACSURL == "" || request.Issuer == "" {
		return authnRequest{}, fmt.Errorf("AuthnRequest lacks ID, ACS URL or Issuer: %s", inflated)
	}
	return request, nil
}

var postForm = template.Must(template.New("post").Parse(`<!doctype html>
<html><body onload="document.forms[0].submit()">
<form method="post" action="{{.ACS}}">
<input type="hidden" name="SAMLResponse" value="{{.Response}}">
{{if .RelayState}}<input type="hidden" name="RelayState" value="{{.RelayState}}">{{end}}
<noscript><button type="submit">Continue</button></noscript>
</form></body></html>`))

func (idp *IdP) sso(w http.ResponseWriter, r *http.Request) {
	request, err := parseRedirectRequest(r.URL.Query().Get("SAMLRequest"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idp.mu.Lock()
	subject, audience := idp.subject, idp.audience
	idp.requests++
	idp.mu.Unlock()
	if subject == "" {
		http.Error(w, "no subject configured", http.StatusConflict)
		return
	}
	if audience == "" {
		audience = request.Issuer
	}
	response, err := idp.response(request, subject, audience, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = postForm.Execute(w, map[string]string{
		"ACS":        request.ACSURL,
		"Response":   base64.StdEncoding.EncodeToString(response),
		"RelayState": r.URL.Query().Get("RelayState"),
	})
}

func newID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return fmt.Sprintf("_%x", raw)
}

// response builds a Response whose Assertion is signed (enveloped, exclusive
// C14N), with the Signature placed after the assertion's Issuer as the SAML
// schema orders it.
func (idp *IdP) response(request authnRequest, subject, audience string, now time.Time) ([]byte, error) {
	instant := now.Format(time.RFC3339)
	expires := now.Add(5 * time.Minute).Format(time.RFC3339)

	assertion := etree.NewElement("saml:Assertion")
	assertion.CreateAttr("xmlns:saml", assertionNS)
	assertion.CreateAttr("ID", newID())
	assertion.CreateAttr("Version", "2.0")
	assertion.CreateAttr("IssueInstant", instant)
	assertion.CreateElement("saml:Issuer").SetText(idp.EntityID)
	subjectElement := assertion.CreateElement("saml:Subject")
	nameID := subjectElement.CreateElement("saml:NameID")
	nameID.CreateAttr("Format", "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress")
	nameID.SetText(subject)
	confirmation := subjectElement.CreateElement("saml:SubjectConfirmation")
	confirmation.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
	data := confirmation.CreateElement("saml:SubjectConfirmationData")
	data.CreateAttr("InResponseTo", request.ID)
	data.CreateAttr("NotOnOrAfter", expires)
	data.CreateAttr("Recipient", request.ACSURL)
	conditions := assertion.CreateElement("saml:Conditions")
	conditions.CreateAttr("NotBefore", now.Add(-time.Minute).Format(time.RFC3339))
	conditions.CreateAttr("NotOnOrAfter", expires)
	conditions.CreateElement("saml:AudienceRestriction").CreateElement("saml:Audience").SetText(audience)
	statement := assertion.CreateElement("saml:AuthnStatement")
	statement.CreateAttr("AuthnInstant", instant)
	statement.CreateAttr("SessionIndex", newID())
	statement.CreateElement("saml:AuthnContext").CreateElement("saml:AuthnContextClassRef").
		SetText("urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport")
	attribute := assertion.CreateElement("saml:AttributeStatement").CreateElement("saml:Attribute")
	attribute.CreateAttr("Name", "email")
	attribute.CreateElement("saml:AttributeValue").SetText(subject)

	signing := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(idp.cert))
	signing.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err := signing.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		return nil, err
	}
	// The schema orders the Signature right after the assertion's Issuer.
	// Build it over the unsigned assertion and insert it there: appending
	// (SignEnveloped) and then moving the element afterwards does not verify.
	signature, err := signing.ConstructSignature(assertion, true)
	if err != nil {
		return nil, fmt.Errorf("sign the assertion: %w", err)
	}
	signed := assertion.Copy()
	signed.InsertChildAt(1, signature)

	response := etree.NewElement("samlp:Response")
	response.CreateAttr("xmlns:samlp", protocolNS)
	response.CreateAttr("xmlns:saml", assertionNS)
	response.CreateAttr("ID", newID())
	response.CreateAttr("Version", "2.0")
	response.CreateAttr("IssueInstant", instant)
	response.CreateAttr("Destination", request.ACSURL)
	response.CreateAttr("InResponseTo", request.ID)
	response.CreateElement("saml:Issuer").SetText(idp.EntityID)
	response.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").
		CreateAttr("Value", "urn:oasis:names:tc:SAML:2.0:status:Success")
	response.AddChild(signed)

	document := etree.NewDocument()
	document.SetRoot(response)
	return document.WriteToBytes()
}

func selfSigned() (tls.Certificate, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "elitea native conformance IdP"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, certPEM, nil
}
