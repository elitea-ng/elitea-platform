package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

type rootSourceVersionExecutor struct {
	*scriptedExecutor
	*currentApplicationNestingQuerierStub
	turn    sqlcgen.ResolveCurrentApplicationTurnRow
	version sqlcgen.ResolveCurrentApplicationVersionDetailsRow
}

func (e *rootSourceVersionExecutor) ResolveCurrentApplicationTurn(_ context.Context, _ sqlcgen.ResolveCurrentApplicationTurnParams) (sqlcgen.ResolveCurrentApplicationTurnRow, error) {
	return e.turn, nil
}
func (e *rootSourceVersionExecutor) ResolveCurrentApplicationVersionDetails(_ context.Context, _ sqlcgen.ResolveCurrentApplicationVersionDetailsParams) (sqlcgen.ResolveCurrentApplicationVersionDetailsRow, error) {
	return e.version, nil
}

type rootSourceProjectStore struct {
	*rootSourceVersionExecutor
	projects []int64
}

func (s *rootSourceProjectStore) WithinProjectTx(ctx context.Context, project int64, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.projects = append(s.projects, project)
	return fn(s.rootSourceVersionExecutor)
}
func rawRootVersionWithNestedSkill() json.RawMessage {
	return json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","status":"published","instructions":"Authored source","meta":{},"tools":[{"id":null,"type":"application","name":"child","settings":{"application_id":101,"application_version_id":1}}]}`)
}
func sourceVersionExecutor(t *testing.T) *rootSourceVersionExecutor {
	t.Helper()
	childName := "child"
	childReference := currentApplicationNestingTestReference(9, 101, 1)
	childReference.ToolName = &childName
	root := currentApplicationNestingTestNode(t, 41, 31, "agent", childReference)
	child := currentApplicationNestingTestNode(t, 1, 101, "agent")
	child.SkillsJson = `[{"skill_id":7,"name":"Deploy","icon_meta":{"icon":"deploy"}}]`
	return &rootSourceVersionExecutor{scriptedExecutor: &scriptedExecutor{}, currentApplicationNestingQuerierStub: &currentApplicationNestingQuerierStub{nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{41: root, 1: child}}, turn: sqlcgen.ResolveCurrentApplicationTurnRow{ApplicationID: 31, ApplicationProjectID: 7, ApplicationVersionID: 41, ApplicationVariablesJson: "[]", ApplicationVersionDetailsJson: string(rawRootVersionWithNestedSkill()), ChatHistoryJson: "[]", InternalToolsJson: "[]"}, version: sqlcgen.ResolveCurrentApplicationVersionDetailsRow{ApplicationID: 31, ApplicationVersionID: 41, ApplicationVersionDetailsJson: string(rawRootVersionWithNestedSkill())}}
}
func TestOriginalRootSourceResolverRetainsRawBeforeNestedSkills(t *testing.T) {
	e := sourceVersionExecutor(t)
	store := &rootSourceProjectStore{rootSourceVersionExecutor: e}
	repo, err := newCurrentAgentStartRepository(store, 1)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveCurrentApplication(t.Context(), app.CurrentApplicationStartRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "question"})
	if err != nil || !bytes.Equal(target.SourceVersionDetails, rawRootVersionWithNestedSkill()) || bytes.Equal(target.SourceVersionDetails, target.VersionDetails) || !bytes.Contains(target.VersionDetails, []byte("nested_skill_registry")) {
		t.Fatalf("source=%s runtime=%s err=%v", target.SourceVersionDetails, target.VersionDetails, err)
	}
	if len(store.projects) != 1 || store.projects[0] != 7 {
		t.Fatal("source escaped authorized project read")
	}
	target.SourceVersionDetails[0] = '!'
	if rawRootVersionWithNestedSkill()[0] != '{' {
		t.Fatal("source carrier aliases the original record")
	}
}
func TestOriginalRootCatalogueSourceRetainsExactAdmittedReadAndOldWrapper(t *testing.T) {
	e := sourceVersionExecutor(t)
	// The existing published catalogue admission forbids tool references.
	e.nodes[41] = currentApplicationNestingTestNode(t, 41, 31, "agent")
	e.version.ApplicationVersionDetailsJson = `{"id":41,"application_id":31,"agent_type":"agent","status":"published","instructions":"Authored catalogue source","meta":{},"tools":[]}`
	catalogueRaw := []byte(e.version.ApplicationVersionDetailsJson)
	store := &rootSourceProjectStore{rootSourceVersionExecutor: e}
	repo, err := newCurrentAgentStartRepository(store, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime, source, err := repo.resolveCatalogueApplicationSourceAndRuntime(t.Context(), currentCatalogueApplicationReference{applicationID: 31, versionID: 41})
	if err != nil || !bytes.Equal(source, catalogueRaw) || bytes.Equal(source, runtime) {
		t.Fatal("catalogue source captured after runtime additions", err)
	}
	old, err := repo.resolveCatalogueApplicationVersion(t.Context(), currentCatalogueApplicationReference{applicationID: 31, versionID: 41})
	if err != nil || !bytes.Equal(old, runtime) || len(store.projects) != 2 || store.projects[0] != 1 || store.projects[1] != 1 {
		t.Fatal("runtime-only catalogue wrapper changed")
	}
}
