package messagetraces

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/google/uuid"
)

// runIdentity is an optional narrowing fence over an existing admitted response.
// Every identity predicate is part of the trace SELECT, never a prior check.
type runIdentity struct {
	executionID, generation, responseID string
	actorID, projectID                  string
}

func parseRunIdentity(r *http.Request) (*runIdentity, error) {
	query := r.URL.Query()
	keys := []string{"execution_id", "execution_generation", "response_message_id"}
	present := false
	for _, key := range keys {
		present = present || query.Has(key)
	}
	if !present {
		return nil, nil
	}
	for _, key := range keys {
		if len(query[key]) != 1 || !validRunIdentity(query.Get(key)) {
			return nil, errors.New("execution_id, execution_generation and response_message_id must be supplied together")
		}
	}
	responseID := query.Get("response_message_id")
	parsed, err := uuid.Parse(responseID)
	if err != nil || parsed.String() != responseID {
		return nil, errors.New("response_message_id must be a canonical UUID")
	}
	return &runIdentity{executionID: query.Get("execution_id"), generation: query.Get("execution_generation"), responseID: responseID}, nil
}

func validRunIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, " \t\r\n\x00")
}

func (identity *runIdentity) bindActor(r *http.Request) error {
	actor, err := chatauthority.Actor(r.Context())
	if err != nil {
		return err
	}
	project, ok := pathID(r, "projectID")
	if !ok {
		return errors.New("invalid trace project")
	}
	identity.actorID = strconv.FormatInt(actor, 10)
	identity.projectID = strconv.FormatInt(project, 10)
	return nil
}

func runIdentityConditions(schema string, identity *runIdentity, args *argList) string {
	if identity == nil {
		return "TRUE"
	}
	executionID := args.add(identity.executionID)
	generation := args.add(identity.generation)
	responseID := args.add(identity.responseID)
	actorID := args.add(identity.actorID)
	projectID := args.add(identity.projectID)
	return fmt.Sprintf(`message_group.task_id=%[1]s AND message_group.meta->>'execution_generation'=%[2]s
 AND message_group.uuid::text=%[3]s AND EXISTS (
 SELECT 1 FROM elitea_runtime.agent_execution_jobs binding
 JOIN elitea_runtime.execution_jobs job ON job.execution_id=binding.execution_id AND job.generation=binding.generation
  AND job.capability_id=binding.capability_id AND job.input_bundle_id=binding.input_bundle_id
 JOIN %[6]s.chat_conversations conversation ON conversation.id=message_group.conversation_id
 WHERE binding.execution_id=%[1]s AND binding.client_execution_generation=%[2]s
  AND binding.client_message_id=%[3]s AND binding.client_stream_id=conversation.uuid::text
  AND job.actor_id=%[4]s AND job.tenant_id=%[5]s
  AND job.resource_project_id=%[5]s::integer AND job.projection_project_id=%[5]s::integer
  AND job.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1'))`, executionID, generation, responseID, actorID, projectID, schema)
}
