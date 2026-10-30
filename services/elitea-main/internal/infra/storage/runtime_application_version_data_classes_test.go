package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type applicationDataClassStub struct {
	classes []string
	calls   [][2]int64
}

func (s *applicationDataClassStub) AgentDataClasses(_ context.Context, projectID, applicationID int64) ([]string, error) {
	s.calls = append(s.calls, [2]int64{projectID, applicationID})
	return s.classes, nil
}

func dataClassVersionService(t *testing.T) *RuntimeApplicationVersionService {
	t.Helper()
	source := currentApplicationVersionSourceFunc(func(context.Context, int64, int64, int64) (CurrentApplicationVersionRecord, error) {
		return CurrentApplicationVersionRecord{
			ApplicationID: 1, VersionID: 2, VersionDetails: json.RawMessage(nestedVersionStoredDetails),
		}, nil
	})
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 90106, ActorID: "11", Initiator: runtimeContextInitiatorUser}, nil
	})
	return nestedApplicationVersionService(t, authorizer, source, newNestedVersionFreezer(t))
}

// ADR-0044 decision 3: a served child carries its declared data classes,
// read for the claim's project, so the worker narrows the delegation to
// them; a child that declares none keeps the envelope unchanged.
func TestNestedApplicationVersionCarriesTheChildsDataClasses(t *testing.T) {
	t.Parallel()
	classes := &applicationDataClassStub{classes: []string{"PII", "finance.restricted"}}
	service := dataClassVersionService(t).WithDataClasses(classes)
	value, err := service.Resolve(context.Background(), ContentClaim{ExecutionID: "execution-1"}, 1, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"pii", "finance.restricted"}, value.DataClasses)
	require.Equal(t, [][2]int64{{90106, 1}}, classes.calls)
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"data_classes":["pii","finance.restricted"]`)

	none := dataClassVersionService(t).WithDataClasses(&applicationDataClassStub{})
	value, err = none.Resolve(context.Background(), ContentClaim{ExecutionID: "execution-1"}, 1, 2)
	require.NoError(t, err)
	encoded, err = json.Marshal(value)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "data_classes")
}

func TestNestedApplicationVersionRefusesAMalformedRegistryRow(t *testing.T) {
	t.Parallel()
	service := dataClassVersionService(t).WithDataClasses(&applicationDataClassStub{classes: []string{"Not A Class"}})
	_, err := service.Resolve(context.Background(), ContentClaim{ExecutionID: "execution-1"}, 1, 2)
	require.Error(t, err, "a malformed class must never be served as none")
}
