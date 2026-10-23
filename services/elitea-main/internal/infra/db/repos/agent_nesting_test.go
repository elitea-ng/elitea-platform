package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

type currentApplicationNestingQuerierStub struct {
	nodes map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow
	err   error
}

func (stub *currentApplicationNestingQuerierStub) ResolveCurrentApplicationNestingNode(
	_ context.Context,
	versionID int32,
) (sqlcgen.ResolveCurrentApplicationNestingNodeRow, error) {
	if stub.err != nil {
		return sqlcgen.ResolveCurrentApplicationNestingNodeRow{}, stub.err
	}
	node, ok := stub.nodes[versionID]
	if !ok {
		return sqlcgen.ResolveCurrentApplicationNestingNodeRow{}, pgx.ErrNoRows
	}
	return node, nil
}

func currentApplicationNestingTestNode(
	t *testing.T,
	versionID int32,
	applicationID int32,
	agentType string,
	children ...currentApplicationNestingReference,
) sqlcgen.ResolveCurrentApplicationNestingNodeRow {
	t.Helper()
	if children == nil {
		children = []currentApplicationNestingReference{}
	}
	encoded, err := json.Marshal(children)
	if err != nil {
		t.Fatal(err)
	}
	return sqlcgen.ResolveCurrentApplicationNestingNodeRow{
		ApplicationVersionID:  versionID,
		ApplicationID:         applicationID,
		AgentType:             agentType,
		SkillsJson:            "[]",
		ChildApplicationsJson: string(encoded),
	}
}

func currentApplicationNestingTestReference(
	toolID int32,
	applicationID int32,
	versionID int32,
) currentApplicationNestingReference {
	return currentApplicationNestingReference{
		ToolID: toolID, ToolName: nil,
		ApplicationID:        json.RawMessage(fmt.Sprintf("%d", applicationID)),
		ApplicationVersionID: json.RawMessage(fmt.Sprintf("%d", versionID)),
	}
}

func TestValidateCurrentApplicationNestingMatchesCurrentTierRules(t *testing.T) {
	tests := []struct {
		name       string
		startDepth int
		nodes      map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow
		wantErr    bool
	}{
		{
			name:       "three agent tiers",
			startDepth: 1,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent", currentApplicationNestingTestReference(12, 102, 2)),
				2: currentApplicationNestingTestNode(t, 2, 102, "agent", currentApplicationNestingTestReference(13, 103, 3)),
				3: currentApplicationNestingTestNode(t, 3, 103, "agent"),
			},
		},
		{
			name:       "fourth agent tier",
			startDepth: 1,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent", currentApplicationNestingTestReference(12, 102, 2)),
				2: currentApplicationNestingTestNode(t, 2, 102, "agent", currentApplicationNestingTestReference(13, 103, 3)),
				3: currentApplicationNestingTestNode(t, 3, 103, "agent", currentApplicationNestingTestReference(14, 104, 4)),
				4: currentApplicationNestingTestNode(t, 4, 104, "agent"),
			},
			wantErr: true,
		},
		{
			name:       "pipelines are transparent",
			startDepth: 1,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent", currentApplicationNestingTestReference(12, 102, 2)),
				2: currentApplicationNestingTestNode(t, 2, 102, "pipeline", currentApplicationNestingTestReference(13, 103, 3)),
				3: currentApplicationNestingTestNode(t, 3, 103, "pipeline", currentApplicationNestingTestReference(14, 104, 4)),
				4: currentApplicationNestingTestNode(t, 4, 104, "agent", currentApplicationNestingTestReference(15, 105, 5)),
				5: currentApplicationNestingTestNode(t, 5, 105, "agent"),
			},
		},
		{
			name:       "ad hoc root starts at tier two",
			startDepth: 2,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent", currentApplicationNestingTestReference(12, 102, 2)),
				2: currentApplicationNestingTestNode(t, 2, 102, "agent"),
			},
		},
		{
			name:       "ad hoc tier three container",
			startDepth: 2,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent", currentApplicationNestingTestReference(12, 102, 2)),
				2: currentApplicationNestingTestNode(t, 2, 102, "agent", currentApplicationNestingTestReference(13, 103, 3)),
				3: currentApplicationNestingTestNode(t, 3, 103, "agent"),
			},
			wantErr: true,
		},
		{
			name:       "path local diamond",
			startDepth: 1,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "agent",
					currentApplicationNestingTestReference(12, 102, 2),
					currentApplicationNestingTestReference(13, 103, 3),
				),
				2: currentApplicationNestingTestNode(t, 2, 102, "agent", currentApplicationNestingTestReference(14, 104, 4)),
				3: currentApplicationNestingTestNode(t, 3, 103, "agent", currentApplicationNestingTestReference(15, 104, 4)),
				4: currentApplicationNestingTestNode(t, 4, 104, "agent"),
			},
		},
		{
			name:       "path cycle",
			startDepth: 1,
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
				1: currentApplicationNestingTestNode(t, 1, 101, "pipeline", currentApplicationNestingTestReference(12, 101, 1)),
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCurrentApplicationNesting(
				context.Background(),
				&currentApplicationNestingQuerierStub{nodes: test.nodes},
				1,
				test.startDepth,
			)
			if test.wantErr && !errors.Is(err, errInvalidCurrentApplicationNesting) {
				t.Fatalf("error=%v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateCurrentApplicationNestingEnforcesRawHopBackstop(t *testing.T) {
	nodes := make(map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow)
	for versionID := int32(1); versionID <= currentMaxApplicationNestingHops+2; versionID++ {
		children := []currentApplicationNestingReference(nil)
		if versionID <= currentMaxApplicationNestingHops+1 {
			children = append(children, currentApplicationNestingTestReference(
				versionID+100,
				versionID+1000,
				versionID+1,
			))
		}
		nodes[versionID] = currentApplicationNestingTestNode(
			t,
			versionID,
			versionID+1000,
			"pipeline",
			children...,
		)
	}
	err := validateCurrentApplicationNesting(
		context.Background(),
		&currentApplicationNestingQuerierStub{nodes: nodes},
		1,
		1,
	)
	if !errors.Is(err, errInvalidCurrentApplicationNesting) {
		t.Fatalf("error=%v", err)
	}
}

func TestFilterCurrentAdhocApplicationNestingSkipsOnlyInvalidApplication(t *testing.T) {
	tools := json.RawMessage(`[
  {"id":19,"type":"github"},
  {"id":null,"type":"application","name":"leaf","settings":{"application_id":101,"application_version_id":1}},
  {"id":null,"type":"application","name":"container","settings":{"application_id":102,"application_version_id":2}}
]`)
	stub := &currentApplicationNestingQuerierStub{nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{
		1: currentApplicationNestingTestNode(t, 1, 101, "agent"),
		2: currentApplicationNestingTestNode(t, 2, 102, "agent", currentApplicationNestingTestReference(23, 103, 3)),
		3: currentApplicationNestingTestNode(t, 3, 103, "agent", currentApplicationNestingTestReference(24, 104, 4)),
		4: currentApplicationNestingTestNode(t, 4, 104, "agent"),
	}}
	filtered, err := filterCurrentAdhocApplicationNesting(context.Background(), stub, tools)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(filtered, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0]["type"] != "github" || decoded[1]["type"] != "application" {
		t.Fatalf("filtered=%s", filtered)
	}
}

func TestFilterCurrentAdhocApplicationNestingFreezesCompactChildSkills(t *testing.T) {
	node := currentApplicationNestingTestNode(t, 1, 101, "agent")
	node.SkillsJson = `[{"skill_id":7,"name":"Deploy","icon_meta":{"icon":"deploy"}}]`
	filtered, err := filterCurrentAdhocApplicationNesting(
		context.Background(),
		&currentApplicationNestingQuerierStub{
			nodes: map[int32]sqlcgen.ResolveCurrentApplicationNestingNodeRow{1: node},
		},
		json.RawMessage(`[
  {"id":null,"type":"application","name":"child-agent","settings":{"application_id":101,"application_version_id":1}}
]`),
	)
	if err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		NestedSkills []currentApplicationSkillRegistry `json:"nested_skill_registry"`
	}
	if err := json.Unmarshal(filtered, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || len(tools[0].NestedSkills) != 1 ||
		tools[0].NestedSkills[0].ApplicationName != "child-agent" ||
		len(tools[0].NestedSkills[0].Skills) != 1 ||
		tools[0].NestedSkills[0].Skills[0].SkillID != 7 ||
		tools[0].NestedSkills[0].Skills[0].Name != "Deploy" ||
		string(tools[0].NestedSkills[0].Skills[0].IconMeta) != `{"icon":"deploy"}` {
		t.Fatalf("materialized tools=%s", filtered)
	}
	if strings.Contains(string(filtered), "instructions") {
		t.Fatalf("nested skill instructions crossed admission: %s", filtered)
	}
}

func TestValidateCurrentApplicationNestingPropagatesQueryFailure(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	err := validateCurrentApplicationNesting(
		context.Background(),
		&currentApplicationNestingQuerierStub{err: databaseErr},
		1,
		1,
	)
	if !errors.Is(err, databaseErr) || errors.Is(err, errInvalidCurrentApplicationNesting) {
		t.Fatalf("error=%v", err)
	}
}
