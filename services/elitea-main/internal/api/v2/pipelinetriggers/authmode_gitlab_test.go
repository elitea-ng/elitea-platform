package pipelinetriggers

// GitLab triggers (legacy issue 6664): the `gitlab` preset, the X-Gitlab-Token
// carrier, and the Standard Webhooks signature a GitLab SIGNING TOKEN produces.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The reference vector the Standard Webhooks specification's own libraries
// test against (standard-webhooks/standard-webhooks, libraries/*). It is not
// computed by the code under test, so a key-derivation or signed-content
// mistake fails here even if signing and verifying agree with each other.
const (
	referenceSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	referenceID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	referenceTimestamp = int64(1614265330)
	referenceBody      = `{"test": 2432232314}`
	referenceSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func standardHeaders(id string, timestamp int64, signature string) http.Header {
	headers := http.Header{}
	if id != "" {
		headers.Set(StandardWebhooksIDHeader, id)
	}
	if timestamp != 0 {
		headers.Set(StandardWebhooksTimestampHeader, strconv.FormatInt(timestamp, 10))
	}
	if signature != "" {
		headers.Set(StandardWebhooksSignatureHeader, signature)
	}
	return headers
}

func signStandard(secret, id string, timestamp int64, body string) string {
	key, _ := standardWebhooksKey(secret)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + strconv.FormatInt(timestamp, 10) + "." + body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestStandardWebhooksSignatureVectors(t *testing.T) {
	signedAt := time.Unix(referenceTimestamp, 0)
	body := []byte(referenceBody)
	for _, test := range []struct {
		name    string
		headers http.Header
		body    []byte
		secret  string
		now     time.Time
		want    bool
	}{
		{"the reference vector", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body, referenceSecret, signedAt, true},
		{"one valid entry among several (key rotation)", standardHeaders(referenceID, referenceTimestamp,
			"v1,bm90LXRoZS1zaWduYXR1cmUtYXQtYWxsLTMyLWJ5dGVz "+referenceSignature), body, referenceSecret, signedAt, true},
		{"an unknown version label is skipped, not trusted", standardHeaders(referenceID, referenceTimestamp,
			"v2,"+strings.TrimPrefix(referenceSignature, "v1,")), body, referenceSecret, signedAt, false},
		{"inside the tolerance, late", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body,
			referenceSecret, signedAt.Add(StandardWebhooksTolerance - time.Second), true},
		{"inside the tolerance, early clock", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body,
			referenceSecret, signedAt.Add(-StandardWebhooksTolerance + time.Second), true},
		{"a replay after the tolerance", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body,
			referenceSecret, signedAt.Add(StandardWebhooksTolerance + time.Second), false},
		{"a timestamp from the future", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body,
			referenceSecret, signedAt.Add(-StandardWebhooksTolerance - time.Second), false},
		{"a tampered body", standardHeaders(referenceID, referenceTimestamp, referenceSignature),
			[]byte(`{"test": 2432232315}`), referenceSecret, signedAt, false},
		{"another message id", standardHeaders("msg_other", referenceTimestamp, referenceSignature), body,
			referenceSecret, signedAt, false},
		{"another secret", standardHeaders(referenceID, referenceTimestamp, referenceSignature), body,
			"whsec_" + base64.StdEncoding.EncodeToString([]byte("a-different-key")), signedAt, false},
		{"no webhook-id", standardHeaders("", referenceTimestamp, referenceSignature), body, referenceSecret, signedAt, false},
		{"no webhook-timestamp", standardHeaders(referenceID, 0, referenceSignature), body, referenceSecret, signedAt, false},
		{"no webhook-signature", standardHeaders(referenceID, referenceTimestamp, ""), body, referenceSecret, signedAt, false},
		{"a non-numeric timestamp", func() http.Header {
			headers := standardHeaders(referenceID, 0, referenceSignature)
			headers.Set(StandardWebhooksTimestampHeader, "yesterday")
			return headers
		}(), body, referenceSecret, signedAt, false},
		{"a signature that is not base64", standardHeaders(referenceID, referenceTimestamp, "v1,%%%"), body,
			referenceSecret, signedAt, false},
		{"a whsec_ secret whose key is not base64", standardHeaders(referenceID, referenceTimestamp, referenceSignature),
			body, "whsec_%%%", signedAt, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := standardWebhooksSignatureMatches(test.headers, test.body, test.secret, test.now); got != test.want {
				t.Fatalf("standardWebhooksSignatureMatches = %v, want %v", got, test.want)
			}
		})
	}
}

// A secret with no `whsec_` prefix is used as its raw bytes, so a sender that
// signs with the string it was given verifies too.
func TestStandardWebhooksAcceptsARawSecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signature := signStandard("plain-shared-secret", "msg_1", now.Unix(), `{}`)
	if !standardWebhooksSignatureMatches(standardHeaders("msg_1", now.Unix(), signature), []byte(`{}`),
		"plain-shared-secret", now) {
		t.Fatal("a raw secret did not verify")
	}
}

// The minted secret for this mode is in the specification's own form, and the
// verifier reads it back the same way. A mismatch between the two would make
// every delivery fail while each function looks right on its own.
func TestStandardWebhooksMintedSecretRoundTrips(t *testing.T) {
	_, secret, hash, err := newCredential(triggerAuthMode{AuthMode: AuthModeStandardWebhooks})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret = %q, want the whsec_ form", secret)
	}
	key, ok := standardWebhooksKey(secret)
	if !ok || len(key) != tokenSecretBytes {
		t.Fatalf("key length = %d (ok=%v), want %d", len(key), ok, tokenSecretBytes)
	}
	if string(hash) != string(secretDigest(secret)) {
		t.Fatal("the stored digest is not the digest of the secret handed out")
	}
	now := time.Now()
	signature := signStandard(secret, "msg_2", now.Unix(), `{"object_kind":"push"}`)
	if !standardWebhooksSignatureMatches(standardHeaders("msg_2", now.Unix(), signature),
		[]byte(`{"object_kind":"push"}`), secret, now) {
		t.Fatal("a delivery signed with the minted secret did not verify")
	}

	_, bearer, _, err := newCredential(defaultAuthMode())
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(bearer, "whsec_") {
		t.Fatal("a bearer trigger's secret changed form")
	}
}

func TestParseAuthModeGitLab(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		want    triggerAuthMode
		wantErr bool
	}{
		{"the gitlab preset is the bearer mode with a gitlab suffix", `{"type":"gitlab"}`,
			triggerAuthMode{AuthMode: AuthModeToken, Provider: ProviderGitLab}, false},
		{"provider is an alias of type", `{"provider":"gitlab"}`,
			triggerAuthMode{AuthMode: AuthModeToken, Provider: ProviderGitLab}, false},
		{"gitlab with a signing token", `{"type":"gitlab","auth_mode":"standard_webhooks_hmac"}`,
			triggerAuthMode{AuthMode: AuthModeStandardWebhooks, SignatureHeader: StandardWebhooksSignatureHeader, Provider: ProviderGitLab}, false},
		{"a generic Standard Webhooks sender", `{"auth_mode":"standard_webhooks_hmac"}`,
			triggerAuthMode{AuthMode: AuthModeStandardWebhooks, SignatureHeader: StandardWebhooksSignatureHeader, Provider: ProviderCustom}, false},
		{"the spec's own header name, in any case", `{"auth_mode":"standard_webhooks_hmac","signature_header":"Webhook-Signature"}`,
			triggerAuthMode{AuthMode: AuthModeStandardWebhooks, SignatureHeader: StandardWebhooksSignatureHeader, Provider: ProviderCustom}, false},
		{"a header a conforming sender never uses", `{"auth_mode":"standard_webhooks_hmac","signature_header":"X-Other"}`,
			triggerAuthMode{}, true},
		{"gitlab signs no raw-body digest", `{"type":"gitlab","auth_mode":"hmac_sha256","signature_header":"X-Sig"}`,
			triggerAuthMode{}, true},
		{"github sends no Standard Webhooks headers", `{"type":"github","auth_mode":"standard_webhooks_hmac"}`,
			triggerAuthMode{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, named, err := parseAuthMode([]byte(test.body))
			if !named {
				t.Fatal("a body naming a mode reported named=false")
			}
			if test.wantErr {
				if !errors.Is(err, errInvalidAuthMode) {
					t.Fatalf("err = %v, want errInvalidAuthMode", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseAuthMode = %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}

// X-Gitlab-Token is a bearer CARRIER (GitLab's secret token is sent verbatim).
// The order is unchanged for the carriers that were there before it.
func TestPresentedSecretReadsTheGitLabTokenHeader(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers map[string]string
		query   string
		want    string
	}{
		{"gitlab header alone", map[string]string{GitLabTokenHeader: "s3cret"}, "", "s3cret"},
		{"bearer wins over gitlab", map[string]string{"Authorization": "Bearer first", GitLabTokenHeader: "second"}, "", "first"},
		{"elitea header wins over gitlab", map[string]string{TriggerTokenHeader: "first", GitLabTokenHeader: "second"}, "", "first"},
		{"gitlab wins over the query string", map[string]string{GitLabTokenHeader: "first"}, "?token=second", "first"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v2/pipeline_trigger/1/abc"+test.query, nil)
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			if got := presentedSecret(request); got != test.want {
				t.Fatalf("presentedSecret = %q, want %q", got, test.want)
			}
		})
	}
}

// Every "is this a signing trigger" branch asks modeSigns, so the new mode
// gets the signing body cap and no secret_url.
func TestStandardWebhooksIsASigningModeEverywhere(t *testing.T) {
	if !modeSigns(AuthModeStandardWebhooks) || !modeSigns(AuthModeHMACSHA256) || modeSigns(AuthModeToken) {
		t.Fatal("modeSigns disagrees with the mode vocabulary")
	}
	large := make([]byte, maxInboundBody+1)
	if !inboundBodyWithinCap(triggerRow{AuthMode: AuthModeStandardWebhooks}, large) {
		t.Fatal("a signed GitLab delivery over the bearer cap is refused")
	}
	view := withSecret(triggerView{URL: "/api/v2/pipeline_trigger/1/abc/gitlab", AuthMode: AuthModeStandardWebhooks}, "whsec_x")
	if view.SecretURL != "" {
		t.Fatalf("a signing trigger was handed a secret_url: %q", view.SecretURL)
	}
	bearer := withSecret(triggerView{URL: "/api/v2/pipeline_trigger/1/abc/gitlab", AuthMode: AuthModeToken}, "s")
	if bearer.SecretURL == "" {
		t.Fatal("a GitLab bearer trigger lost its secret_url")
	}
}
