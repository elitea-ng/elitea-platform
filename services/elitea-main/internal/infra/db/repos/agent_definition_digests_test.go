package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// #1462: the transaction binds a binding only when the freeze's computed
// digest is the one it pins. A freeze that did not check, or no freeze,
// compares nothing (the earlier behaviour).
func TestDefinitionMismatchComparesOnlyACheckedFreeze(t *testing.T) {
	bound := runtimedomain.Digest{1}
	for name, c := range map[string]struct {
		governed *agentexecutionapp.GovernedCredentialFreeze
		want     bool
	}{
		"no freeze":       {nil, false},
		"not checked":     {&agentexecutionapp.GovernedCredentialFreeze{DefinitionDigest: [32]byte{2}}, false},
		"matching":        {&agentexecutionapp.GovernedCredentialFreeze{DefinitionChecked: true, DefinitionDigest: [32]byte{1}}, false},
		"other digest":    {&agentexecutionapp.GovernedCredentialFreeze{DefinitionChecked: true, DefinitionDigest: [32]byte{2}}, true},
		"version unknown": {&agentexecutionapp.GovernedCredentialFreeze{DefinitionChecked: true}, true},
	} {
		if got := definitionMismatch(securityContextFacts{governed: c.governed}, bound); got != c.want {
			t.Errorf("%s: mismatch = %v, want %v", name, got, c.want)
		}
	}
}

type versionReaderStub struct {
	record storage.CurrentApplicationVersionRecord
	err    error
	asked  [3]int64
}

func (s *versionReaderStub) ReadCurrentApplicationVersion(_ context.Context, project, application, version int64) (storage.CurrentApplicationVersionRecord, error) {
	s.asked = [3]int64{project, application, version}
	return s.record, s.err
}

// The digest is VersionDefinitionDigest of the stored projection; an
// absent version is found=false, never an error; a read failure is an error.
func TestAgentDefinitionDigestsDigestTheStoredVersion(t *testing.T) {
	definition := []byte(`{"id":4,"application_id":3,"instructions":"x"}`)
	reader := &versionReaderStub{record: storage.CurrentApplicationVersionRecord{ApplicationID: 3, VersionID: 4, VersionDetails: definition}}
	digests := &AgentDefinitionDigests{versions: reader}
	got, found, err := digests.DefinitionDigest(context.Background(), 7, 3, 4)
	want, _ := agentidentity.VersionDefinitionDigest(7, 3, 4, definition)
	if err != nil || !found || got != want || reader.asked != [3]int64{7, 3, 4} {
		t.Fatalf("digest %x found %v err %v asked %v", got, found, err, reader.asked)
	}
	reader.err = storage.ErrContentNotFound
	if _, found, err := digests.DefinitionDigest(context.Background(), 7, 3, 4); found || err != nil {
		t.Fatalf("absent: found %v err %v", found, err)
	}
	reader.err = errors.New("database down")
	if _, _, err := digests.DefinitionDigest(context.Background(), 7, 3, 4); err == nil {
		t.Fatal("a read failure was not an error")
	}
	if _, found, err := digests.DefinitionDigest(context.Background(), 0, 3, 4); found || err != nil {
		t.Fatalf("no project: found %v err %v", found, err)
	}
	if err := digests.RecordDefinitionMismatch(context.Background(), 7, "b", 3, 4); err == nil {
		t.Fatal("a mismatch was recorded with no audit appender")
	}
}
