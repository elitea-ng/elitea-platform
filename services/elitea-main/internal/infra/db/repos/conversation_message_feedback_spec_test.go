package repos

import (
	"errors"
	"strconv"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/EliteaAI/elitea-platform/services/elitea-main/api/openapi"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// TestMessageFeedbackSpecDeclaresTheRepositoryNotFound: getMessageFeedback
// and deleteMessageFeedback are client-tagged in contract 1.3, and the
// handler hands the repository's error to apierr.Write unchanged. Both reads
// answer 404 for a message id that is not a uuid (the GET also for a message
// this project does not have, for example a turn a Stop removed on another
// device), so the locked responses must carry that status, as
// setMessageFeedback's already do. The malformed-id branch returns before
// the pool is touched, so a zero repository exercises it.
func TestMessageFeedbackSpecDeclaresTheRepositoryNotFound(t *testing.T) {
	repo := &ConversationsRepo{}
	calls := map[string]func() error{
		"getMessageFeedback": func() error {
			_, err := repo.GetMessageFeedback(t.Context(), "2", "not-a-uuid", "7")
			return err
		},
		"deleteMessageFeedback": func() error {
			_, err := repo.DeleteMessageFeedback(t.Context(), "2", "not-a-uuid", "7")
			return err
		},
	}

	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]*openapi3.Operation{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			ops[op.OperationID] = op
		}
	}
	for operationID, call := range calls {
		var apiErr *apierr.APIError
		if err := call(); !errors.As(err, &apiErr) {
			t.Fatalf("%s: error=%v, want an apierr", operationID, err)
		}
		op := ops[operationID]
		if op == nil {
			t.Fatalf("v2.yaml has no %s operation", operationID)
		}
		if op.Responses.Value(strconv.Itoa(apiErr.Status)) == nil {
			t.Errorf("%s answers %d (%s) but v2.yaml does not declare it", operationID, apiErr.Status, apiErr.Message)
		}
	}
}
