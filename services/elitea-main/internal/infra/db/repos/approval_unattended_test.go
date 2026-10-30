package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The owner notification id of a wait is reviewNotificationUUID over a
// prefixed key: the ids stay what they were and stay apart from the
// credential review ids.
func TestApprovalNotificationIDIsThePrefixedReviewID(t *testing.T) {
	key := "execution-1\x00interrupt-1"
	digest := sha256.Sum256([]byte("elitea.approval-notification.v1\x00" + key))
	value := append([]byte(nil), digest[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	want := encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
	if got := reviewNotificationUUID(approvalNotificationKeyPrefix + key); got != want {
		t.Fatalf("id = %s, want %s", got, want)
	}
	if reviewNotificationUUID(key) == want {
		t.Fatal("the prefix does not separate the ids")
	}
}

func TestUnattendedIntentNeedsAnOwnerAConversationAndAResponse(t *testing.T) {
	entry := approvalEntry{ConversationUUID: "10000000-0000-4000-8000-000000000001", ResponseMessageID: "response-1"}
	if !canKeepUnattendedIntent(entry, unattendedPolicy{OwnerUserID: 8}) {
		t.Fatal("a complete intent was refused")
	}
	if canKeepUnattendedIntent(entry, unattendedPolicy{}) {
		t.Fatal("an intent without an owner was kept")
	}
	noConversation := entry
	noConversation.ConversationUUID = "stream-1"
	noResponse := entry
	noResponse.ResponseMessageID = ""
	for _, broken := range []approvalEntry{noConversation, noResponse} {
		if canKeepUnattendedIntent(broken, unattendedPolicy{OwnerUserID: 8}) {
			t.Fatalf("intent %+v was kept", broken)
		}
	}
}
