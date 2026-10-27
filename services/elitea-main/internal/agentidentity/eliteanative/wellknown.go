package eliteanative

import (
	"encoding/json"
	"net/http"
	"time"
)

// wellKnownMaxAge is how long a cache may keep either document. It is far
// below the rotation overlap, so a relying party sees a new key long before
// it signs.
const wellKnownMaxAge = "public, max-age=300"

// WellKnown serves the issuer's public documents: the JWKS and the issuer
// metadata. It holds the public half of the ring only; nothing here signs.
// Neither document carries tenant data.
type WellKnown struct {
	ring       *KeyRing
	issuer     string
	jwksURI    string
	algorithms []Algorithm
	now        func() time.Time
}

// ServeJWKS answers GET JWKSPath.
func (w *WellKnown) ServeJWKS(rw http.ResponseWriter, _ *http.Request) {
	body, err := w.ring.JWKS(w.now())
	if err != nil {
		rw.Header().Set("Cache-Control", "no-store")
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	write(rw, "application/jwk-set+json", body)
}

// metadata is the subset of OpenID Provider Metadata a federating relying
// party reads (AWS IAM, GCP WIF and Vault discover the JWKS through it).
type metadata struct {
	Issuer            string   `json:"issuer"`
	JWKSURI           string   `json:"jwks_uri"`
	SigningAlgorithms []string `json:"id_token_signing_alg_values_supported"`
	ResponseTypes     []string `json:"response_types_supported"`
	SubjectTypes      []string `json:"subject_types_supported"`
	ClaimsSupported   []string `json:"claims_supported"`
	AssertionTypes    []string `json:"elitea_assertion_types_supported"`
}

// ServeMetadata answers GET MetadataPath.
func (w *WellKnown) ServeMetadata(rw http.ResponseWriter, _ *http.Request) {
	algorithms := make([]string, 0, len(w.algorithms))
	for _, a := range w.algorithms {
		algorithms = append(algorithms, string(a))
	}
	body, err := json.Marshal(metadata{
		Issuer: w.issuer, JWKSURI: w.jwksURI, SigningAlgorithms: algorithms,
		ResponseTypes: []string{"id_token"}, SubjectTypes: []string{"public"},
		ClaimsSupported: []string{"iss", "sub", "aud", "iat", "nbf", "exp", "jti", "sub_profile", "agent_instance_id",
			"elitea_tenant", "elitea_project", "elitea_binding", "elitea_version_digest", "elitea_policy_revision",
			"elitea_epoch", "elitea_user"},
		AssertionTypes: []string{TokenType},
	})
	if err != nil {
		rw.Header().Set("Cache-Control", "no-store")
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	write(rw, "application/json", body)
}

func write(rw http.ResponseWriter, contentType string, body []byte) {
	rw.Header().Set("Content-Type", contentType)
	rw.Header().Set("Cache-Control", wellKnownMaxAge)
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(body)
}
