package conversations

import (
	"encoding/json"
	"reflect"
	"strconv"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const EditorTestSource = "editor_test"

type EditorTestContext struct {
	Revision             int    `json:"revision"`
	ActorID              string `json:"actor_id"`
	ProjectID            string `json:"project_id"`
	ApplicationID        string `json:"application_id"`
	ApplicationVersionID string `json:"application_version_id"`
}

type EditorTestInputReference struct {
	BundleID         string `json:"bundle_id"`
	EntryID          string `json:"entry_id"`
	ImmutableVersion string `json:"immutable_version"`
	ContentDigest    string `json:"content_digest"`
}

type EditorTestRun struct {
	ResponseMessageGroupID int64                    `json:"response_message_group_id"`
	TraceAvailable         bool                     `json:"trace_available"`
	ResponseMessageID      string                   `json:"response_message_id"`
	QuestionID             string                   `json:"question_id"`
	ExecutionID            string                   `json:"execution_id"`
	ExecutionGeneration    string                   `json:"execution_generation"`
	Phase                  string                   `json:"phase"`
	State                  string                   `json:"state"`
	DesiredState           string                   `json:"desired_state"`
	AdmittedAt             time.Time                `json:"admitted_at"`
	SettledAt              *time.Time               `json:"settled_at"`
	InputReference         EditorTestInputReference `json:"input_reference"`
	CanControl             bool                     `json:"can_control"`
	EventsURL              string                   `json:"events_url,omitempty"`
}

type EditorTestRunsPage struct {
	Rows    []EditorTestRun `json:"rows"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	HasMore bool            `json:"has_more"`
}

// EditorTestIdentity accepts one saved application. Actor and project are server authority.
func EditorTestIdentity(conv Conversation, actorID int64, projectID string) (EditorTestContext, error) {
	fail := func() (EditorTestContext, error) {
		return EditorTestContext{}, apierr.BadRequest("invalid editor Test identity")
	}
	if actorID <= 0 || positiveID(projectID) == "" || len(conv.Participants) != 1 {
		return fail()
	}
	if _, supplied := conv.Meta[EditorTestSource]; supplied {
		return fail()
	}
	participant := conv.Participants[0]
	if participant.EntityName != "application" {
		return fail()
	}
	applicationID := positiveID(participant.EntityMeta["id"])
	participantProject := positiveID(participant.EntityMeta["project_id"])
	versionID := positiveID(participant.EntitySettings["version_id"])
	if applicationID == "" || participantProject != projectID || versionID == "" {
		return fail()
	}
	return EditorTestContext{Revision: 1, ActorID: strconv.FormatInt(actorID, 10), ProjectID: projectID, ApplicationID: applicationID, ApplicationVersionID: versionID}, nil
}

func positiveID(raw any) string {
	var value string
	switch n := raw.(type) {
	case string:
		value = n
	case int:
		value = strconv.Itoa(n)
	case int64:
		value = strconv.FormatInt(n, 10)
	case float64:
		if n != float64(int64(n)) {
			return ""
		}
		value = strconv.FormatInt(int64(n), 10)
	case json.Number:
		value = string(n)
	default:
		return ""
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
		return ""
	}
	return value
}

// PreserveEditorTestUpdate prevents generic settings updates from replacing Test authority.
func PreserveEditorTestUpdate(current Conversation, update *Conversation) error {
	if current.Source != EditorTestSource {
		if _, present := update.Meta[EditorTestSource]; present {
			return apierr.BadRequest("editor Test identity is server owned")
		}
		return nil
	}
	if update.IsPrivate != nil && !*update.IsPrivate {
		return apierr.BadRequest("editor Test conversations remain private")
	}
	if update.Meta == nil {
		return nil
	}
	for _, key := range []string{EditorTestSource, "single_participant", "is_hidden"} {
		if supplied, present := update.Meta[key]; present && !reflect.DeepEqual(supplied, current.Meta[key]) {
			return apierr.BadRequest("editor Test identity is immutable")
		}
	}
	preserved := make(map[string]any, len(update.Meta)+3)
	for key, value := range update.Meta {
		preserved[key] = value
	}
	for _, key := range []string{EditorTestSource, "single_participant", "is_hidden"} {
		preserved[key] = current.Meta[key]
	}
	update.Meta = preserved
	return nil
}
