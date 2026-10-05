package messagetraces

import (
	"net/http/httptest"
	"testing"
)

func TestRunIdentityRequiresCompleteNarrowingFence(t *testing.T) {
	const response = "20000000-0000-4000-8000-000000000001"
	for _, query := range []string{
		"execution_id=run", "execution_id=&execution_generation=g&response_message_id=" + response,
		"execution_id=run&execution_generation=g&response_message_id=invalid",
		"execution_id=run&execution_id=other&execution_generation=g&response_message_id=" + response,
		"execution_id=run%0A&execution_generation=g&response_message_id=" + response,
	} {
		if _, err := parseRunIdentity(httptest.NewRequest("GET", "/?"+query, nil)); err == nil {
			t.Fatalf("accepted incomplete/invalid fence %q", query)
		}
	}
	request := httptest.NewRequest("GET", "/?execution_id=run&execution_generation=g&response_message_id="+response, nil)
	if _, err := parseListQuery(request); err == nil {
		t.Fatal("run fence broadened without message group")
	}
	identity, err := parseRunIdentity(request)
	if err != nil || identity == nil || identity.responseID != response {
		t.Fatal("complete fence refused", err)
	}
	if identity, err := parseRunIdentity(httptest.NewRequest("GET", "/", nil)); err != nil || identity != nil {
		t.Fatal("ordinary read changed")
	}
}
