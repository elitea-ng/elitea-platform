package samlidp

import (
	"bytes"
	"compress/flate"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

func redirectRequest(t *testing.T, id, issuer, acs string) string {
	t.Helper()
	xml := `<samlp:AuthnRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="` +
		id + `" Version="2.0" AssertionConsumerServiceURL="` + acs + `"><saml:Issuer>` + issuer + `</saml:Issuer></samlp:AuthnRequest>`
	var buffer bytes.Buffer
	writer, _ := flate.NewWriter(&buffer, flate.DefaultCompression)
	_, _ = writer.Write([]byte(xml))
	_ = writer.Close()
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

var hiddenValue = regexp.MustCompile(`name="SAMLResponse" value="([^"]+)"`)

func TestTheAssertionIsSignedAndAnswersTheRequest(t *testing.T) {
	idp, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idp.Close() }()
	idp.SignInAs("ann@example.com", "")

	query := url.Values{"SAMLRequest": {redirectRequest(t, "_req1", "https://sp.example/metadata", "https://sp.example/acs")},
		"RelayState": {"rs"}}
	response, err := http.Get(idp.SSOURL + "?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	match := hiddenValue.FindSubmatch(body)
	if response.StatusCode != http.StatusOK || match == nil || !bytes.Contains(body, []byte(`action="https://sp.example/acs"`)) {
		t.Fatalf("HTTP %d: %s", response.StatusCode, body)
	}
	raw, err := base64.StdEncoding.DecodeString(html.UnescapeString(string(match[1])))
	if err != nil {
		t.Fatal(err)
	}
	document := etree.NewDocument()
	if err := document.ReadFromBytes(raw); err != nil {
		t.Fatal(err)
	}
	root := document.Root()
	if root.SelectAttrValue("InResponseTo", "") != "_req1" || root.SelectAttrValue("Destination", "") != "https://sp.example/acs" {
		t.Fatalf("response attributes: %s", raw)
	}
	assertion := root.SelectElement("Assertion")
	if assertion == nil {
		t.Fatalf("no assertion: %s", raw)
	}
	if children := assertion.ChildElements(); len(children) < 2 || children[1].Tag != "Signature" {
		t.Fatalf("the Signature is not the assertion's second child: %s", raw)
	}
	if audience := assertion.FindElement(".//Audience"); audience == nil || audience.Text() != "https://sp.example/metadata" {
		t.Fatalf("audience: %s", raw)
	}

	block, _ := pem.Decode([]byte(idp.CertificatePEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	validation := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{cert}})
	if _, err := validation.Validate(assertion); err != nil {
		t.Fatalf("the assertion signature does not verify: %v", err)
	}
	// Tampering breaks it.
	assertion.FindElement(".//NameID").SetText("mallory@example.com")
	if _, err := validation.Validate(assertion); err == nil {
		t.Fatal("a tampered assertion verified")
	}
	if idp.Requests() != 1 {
		t.Fatalf("requests %d", idp.Requests())
	}
}

func TestAnAudienceOverrideIsHonoured(t *testing.T) {
	idp, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idp.Close() }()
	idp.SignInAs("ann@example.com", "urn:someone-else")
	raw, err := idp.response(authnRequest{ID: "_r", Issuer: "https://sp.example", ACSURL: "https://sp.example/acs"},
		"ann@example.com", "urn:someone-else", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("<saml:Audience>urn:someone-else</saml:Audience>")) {
		t.Fatalf("audience not overridden: %s", raw)
	}
}

func TestMalformedRequestsAreRefused(t *testing.T) {
	for _, encoded := range []string{"", "%%%", base64.StdEncoding.EncodeToString([]byte("not deflate"))} {
		if _, err := parseRedirectRequest(encoded); err == nil {
			t.Errorf("%q accepted", encoded)
		}
	}
}
