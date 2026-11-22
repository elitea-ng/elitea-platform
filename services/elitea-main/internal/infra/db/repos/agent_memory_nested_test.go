package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type stubNestedVersionSource struct {
	projectID int64
	versions  map[[2]int64]json.RawMessage
	err       error
}

func (s stubNestedVersionSource) ReadCurrentApplicationVersion(
	_ context.Context,
	projectID, applicationID, versionID int64,
) (storage.CurrentApplicationVersionRecord, error) {
	if s.err != nil {
		return storage.CurrentApplicationVersionRecord{}, s.err
	}
	if projectID != s.projectID {
		return storage.CurrentApplicationVersionRecord{}, storage.ErrContentNotFound
	}
	raw, ok := s.versions[[2]int64{applicationID, versionID}]
	if !ok {
		return storage.CurrentApplicationVersionRecord{}, storage.ErrContentNotFound
	}
	return storage.CurrentApplicationVersionRecord{
		ApplicationID:  applicationID,
		VersionID:      versionID,
		VersionDetails: raw,
	}, nil
}

func TestResolveReachableNestedVersionEnforcesCompositionDepthProjectAndCycles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := stubNestedVersionSource{
		projectID: 7,
		versions: map[[2]int64]json.RawMessage{
			{20, 200}: json.RawMessage(`{"meta":{"memory":{"enabled":true,"write":"auto_attended"}},"tools":[{"type":"application","settings":{"application_id":30,"application_version_id":300}}]}`),
			{30, 300}: json.RawMessage(`{"meta":{"memory":{"enabled":true,"write":"off"}},"tools":[{"type":"application","settings":{"application_id":20,"application_version_id":200}}]}`),
			{99, 999}: json.RawMessage(`{"meta":{"memory":{"enabled":true,"write":"auto"}}}`),
		},
	}
	rootInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"id":10,"version_id":100,"version_details":{"tools":[
			{"type":"application","settings":{"application_id":20,"application_version_id":200}},
			{"type":"application","settings":{"project_id":8,"application_id":99,"application_version_id":999}},
			{"type":"application","settings":{"project_id":"9","application_id":99,"application_version_id":999}}
		]}}`),
	}

	// Depth 1 direct child is reachable and yields its own meta.memory.
	details, err := resolveReachableNestedVersion(ctx, source, 7, rootInput, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}, 4)
	if err != nil {
		t.Fatalf("depth 1 reachable: %v", err)
	}
	settings, err := versionDetailsMemorySettings(details)
	if err != nil || !settings.Enabled || settings.Write != agentmemory.WriteAutoAttended {
		t.Fatalf("depth 1 settings = %+v, %v", settings, err)
	}

	// Depth 2 grandchild is reachable when maxDepth >= 2, refused when maxDepth == 1.
	details, err = resolveReachableNestedVersion(ctx, source, 7, rootInput, storage.AgentMemoryTarget{ApplicationID: 30, VersionID: 300}, 2)
	if err != nil {
		t.Fatalf("depth 2 reachable: %v", err)
	}
	settings, err = versionDetailsMemorySettings(details)
	if err != nil || !settings.Enabled || settings.Write != agentmemory.WriteOff {
		t.Fatalf("depth 2 settings = %+v, %v", settings, err)
	}
	if _, err := resolveReachableNestedVersion(ctx, source, 7, rootInput, storage.AgentMemoryTarget{ApplicationID: 30, VersionID: 300}, 1); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("depth 2 past maxDepth=1: want ErrContentUnauthorized, got %v", err)
	}

	// Unreachable same-project target and foreign-project references fail closed (403, no oracle).
	for _, target := range []storage.AgentMemoryTarget{
		{ApplicationID: 99, VersionID: 999}, // unreachable / foreign project_id
		{ApplicationID: 20, VersionID: 201}, // wrong version id
		{ApplicationID: 77, VersionID: 777}, // absent
	} {
		if _, err := resolveReachableNestedVersion(ctx, source, 7, rootInput, target, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
			t.Fatalf("target %+v: want ErrContentUnauthorized, got %v", target, err)
		}
	}

	// Ad-hoc root reads from input.Tools.
	adhocInput := &runtimev1.AgentExecutionInputV1{
		Tools: []byte(`[{"type":"application","settings":{"application_id":20,"application_version_id":200}}]`),
	}
	if _, err := resolveReachableNestedVersion(ctx, source, 7, adhocInput, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}, 4); err != nil {
		t.Fatalf("adhoc child reachable: %v", err)
	}

	// Bound: more than maxNestedAgentMemoryVersions visited fails closed.
	var manyTools []map[string]any
	manyVersions := make(map[[2]int64]json.RawMessage, maxNestedAgentMemoryVersions+2)
	for i := int64(1); i <= maxNestedAgentMemoryVersions+1; i++ {
		manyTools = append(manyTools, map[string]any{
			"type":     "application",
			"settings": map[string]any{"application_id": i, "application_version_id": i},
		})
		manyVersions[[2]int64{i, i}] = json.RawMessage(`{"tools":[]}`)
	}
	rawMany, _ := json.Marshal(manyTools)
	overLimitInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(fmt.Sprintf(`{"id":1000,"version_id":1000,"version_details":{"tools":%s}}`, rawMany)),
	}
	overSource := stubNestedVersionSource{projectID: 7, versions: manyVersions}
	if _, err := resolveReachableNestedVersion(ctx, overSource, 7, overLimitInput, storage.AgentMemoryTarget{
		ApplicationID: maxNestedAgentMemoryVersions + 1, VersionID: maxNestedAgentMemoryVersions + 1,
	}, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("limit+1 target: want ErrContentUnauthorized, got %v", err)
	}
}

func TestDelegatedReasonPreservesProvenance(t *testing.T) {
	t.Parallel()
	if got := delegatedReason("pending", 0); got != "pending" {
		t.Fatalf("root reason = %q", got)
	}
	if got := delegatedReason("pending", 11); got != "pending,delegated_from:11" {
		t.Fatalf("nested reason = %q", got)
	}
	if got := delegatedReason("", 11); got != "delegated_from:11" {
		t.Fatalf("empty base nested reason = %q", got)
	}
}
