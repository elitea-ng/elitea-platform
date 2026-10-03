package pipelinetriggers

import (
	"net/http"
	"testing"
)

// The GitLab SECRET-TOKEN preset is a bearer mode whose body is GitLab's own
// payload, so it gets the provider cap; a custom bearer trigger keeps 64 KiB.
func TestTheBodyCapFollowsWhoShapesTheBody(t *testing.T) {
	large := make([]byte, maxInboundBody+1)
	for _, test := range []struct {
		name    string
		trigger triggerRow
		want    bool
	}{
		{"gitlab secret token", triggerRow{AuthMode: AuthModeToken, Provider: ProviderGitLab}, true},
		{"gitlab signing token", triggerRow{AuthMode: AuthModeStandardWebhooks, Provider: ProviderGitLab}, true},
		{"github", triggerRow{AuthMode: AuthModeHMACSHA256, Provider: ProviderGitHub}, true},
		{"custom hmac", triggerRow{AuthMode: AuthModeHMACSHA256, Provider: ProviderCustom}, true},
		{"custom bearer", triggerRow{AuthMode: AuthModeToken, Provider: ProviderCustom}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := inboundBodyWithinCap(test.trigger, large); got != test.want {
				t.Fatalf("inboundBodyWithinCap over 64 KiB = %v, want %v", got, test.want)
			}
		})
	}
	if !inboundBodyWithinCap(triggerRow{AuthMode: AuthModeToken, Provider: ProviderCustom}, make([]byte, maxInboundBody)) {
		t.Fatal("a custom bearer body AT the cap was refused")
	}
}

func headerOf(pairs ...string) http.Header {
	header := http.Header{}
	for index := 0; index+1 < len(pairs); index += 2 {
		header.Set(pairs[index], pairs[index+1])
	}
	return header
}

// The delivery key is built from what the sender SIGNED: the webhook-id for
// Standard Webhooks, the raw body for hmac_sha256. Unsigned headers do not
// move it, and a bearer trigger has none.
func TestSignedDeliveryKeyUsesOnlySignedMaterial(t *testing.T) {
	standard := triggerRow{AuthMode: AuthModeStandardWebhooks}
	first := signedDeliveryKey(standard, headerOf(StandardWebhooksIDHeader, "msg_1"), []byte("a"))
	if len(first) != 64 {
		t.Fatalf("key %q is not a 64-character digest", first)
	}
	if signedDeliveryKey(standard, headerOf(StandardWebhooksIDHeader, " msg_1 "), []byte("b")) != first {
		t.Fatal("the Standard Webhooks key moved with the body or with padding; it is the verified webhook-id")
	}
	if signedDeliveryKey(standard, headerOf(StandardWebhooksIDHeader, "msg_2"), []byte("a")) == first {
		t.Fatal("two webhook-ids share a key")
	}
	if signedDeliveryKey(standard, headerOf(), []byte("a")) != "" {
		t.Fatal("no webhook-id must give no key")
	}

	github := triggerRow{AuthMode: AuthModeHMACSHA256}
	body := []byte(`{"after":"0a1b"}`)
	key := signedDeliveryKey(github, headerOf("X-GitHub-Delivery", "one"), body)
	if signedDeliveryKey(github, headerOf("X-GitHub-Delivery", "two"), body) != key {
		t.Fatal("an unsigned X-GitHub-Delivery moved the key — a replay could change it at will")
	}
	if signedDeliveryKey(github, headerOf(), []byte(`{"after":"0a1c"}`)) == key {
		t.Fatal("two bodies share a key")
	}
	// The two key spaces are separate even for identical material.
	if signedDeliveryKey(github, headerOf(), []byte("msg_1")) ==
		signedDeliveryKey(standard, headerOf(StandardWebhooksIDHeader, "msg_1"), nil) {
		t.Fatal("the two modes' key spaces collide")
	}
	if signedDeliveryKey(triggerRow{AuthMode: AuthModeToken}, headerOf(StandardWebhooksIDHeader, "msg_1"), body) != "" {
		t.Fatal("a bearer trigger was given a delivery key")
	}
}
