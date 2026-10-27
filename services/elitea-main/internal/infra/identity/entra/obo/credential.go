package obo

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
)

const clientAssertionLifetime = 5 * time.Minute

// ClientCredential authenticates the middle-tier application at the token
// endpoint. Certificate and federated credentials are the governed default;
// a client secret is accepted only when ClassicConfig.AllowClientSecret says
// the policy revision allows it.
type ClientCredential interface {
	// authenticate adds the client authentication parameters to form.
	authenticate(ctx context.Context, clientID, tokenURL string, now time.Time, form url.Values) error
	isSecret() bool
}

// FederatedCredential authenticates with an externally issued assertion
// (workload identity federation). source is called on every exchange, so a
// rotated projected token is always current.
func FederatedCredential(source func(ctx context.Context) (string, error)) ClientCredential {
	return federatedCredential{source: source}
}

type federatedCredential struct {
	source func(ctx context.Context) (string, error)
}

func (c federatedCredential) authenticate(ctx context.Context, _, _ string, _ time.Time, form url.Values) error {
	if c.source == nil {
		return ErrInvalidConfiguration
	}
	assertion, err := c.source(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrUnavailable
	}
	assertion = strings.TrimSpace(assertion)
	if assertion == "" || len(assertion) > maxAssertionBytes || strings.ContainsFunc(assertion, unicode.IsControl) {
		return ErrUnavailable
	}
	form.Set("client_assertion_type", clientAssertionTypeJWT)
	form.Set("client_assertion", assertion)
	return nil
}

func (federatedCredential) isSecret() bool { return false }

// CertificateCredential authenticates with a client assertion signed by the
// application's certificate key (PS256, x5t#S256 header).
func CertificateCredential(certificate *x509.Certificate, key *rsa.PrivateKey) ClientCredential {
	return certificateCredential{certificate: certificate, key: key}
}

type certificateCredential struct {
	certificate *x509.Certificate
	key         *rsa.PrivateKey
}

func (c certificateCredential) authenticate(_ context.Context, clientID, tokenURL string, now time.Time, form url.Values) error {
	if c.certificate == nil || c.key == nil {
		return ErrInvalidConfiguration
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return ErrUnavailable
	}
	thumbprint := sha256.Sum256(c.certificate.Raw)
	token := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
		"aud": tokenURL,
		"iss": clientID,
		"sub": clientID,
		"jti": base64.RawURLEncoding.EncodeToString(jti[:]),
		"nbf": now.Unix(),
		"iat": now.Unix(),
		"exp": now.Add(clientAssertionLifetime).Unix(),
	})
	token.Header["x5t#S256"] = base64.RawURLEncoding.EncodeToString(thumbprint[:])
	signed, err := token.SignedString(c.key)
	if err != nil {
		return ErrInvalidConfiguration
	}
	form.Set("client_assertion_type", clientAssertionTypeJWT)
	form.Set("client_assertion", signed)
	return nil
}

func (certificateCredential) isSecret() bool { return false }

// ClientSecretCredential authenticates with a shared secret. Governed
// deployments use it only when the policy revision allows it.
func ClientSecretCredential(secret string) ClientCredential {
	return secretCredential{secret: secret}
}

type secretCredential struct {
	secret string
}

func (c secretCredential) authenticate(_ context.Context, _, _ string, _ time.Time, form url.Values) error {
	if c.secret == "" || len(c.secret) > maxConfigurationTextLen*8 {
		return ErrInvalidConfiguration
	}
	form.Set("client_secret", c.secret)
	return nil
}

func (secretCredential) isSecret() bool { return true }

// String keeps the secret out of %v.
func (secretCredential) String() string { return "obo.ClientSecretCredential{[redacted]}" }

// GoString keeps the secret out of %#v.
func (c secretCredential) GoString() string { return c.String() }
