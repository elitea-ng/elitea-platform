package toolkitcalltool

import (
	"context"
	"errors"
	"testing"
)

type recoveryStore struct {
	resultStore
	scope, key  string
	executionID string
	err         error
}

func (s *recoveryStore) FindToolkitCallToolExecution(_ context.Context, scope, key string) (string, error) {
	s.scope, s.key = scope, key
	return s.executionID, s.err
}

func TestRecoverToolRunBeforeExecutionIDReceipt(t *testing.T) {
	store := &recoveryStore{executionID: "accepted-execution", resultStore: resultStore{state: "RUNNING"}}
	service := &RunService{settlements: store}
	request := ResultRequest{ProjectID: 1, ActorUserID: 7, ToolkitID: 19, RequestKey: "browser-request"}
	result, pending, err := service.ReadToolRun(context.Background(), request)
	if err != nil || !pending || result.ExecutionID != store.executionID {
		t.Fatalf("result=%+v pending=%v err=%v", result, pending, err)
	}
	run := validRequest()
	run.IdempotencyKey = request.RequestKey
	key, err := service.idempotencyKey(run, testInputs())
	if err != nil || store.scope != "1/1/7" || store.key != key {
		t.Fatalf("admission and lookup identities differ: %v", err)
	}
	store.err = ErrToolRunNotFound
	if _, _, err := service.ReadToolRun(context.Background(), request); !errors.Is(err, ErrToolRunNotFound) {
		t.Fatalf("unaccepted request: %v", err)
	}
	store.err = nil
	store.denied = true
	if _, _, err := service.ReadToolRun(context.Background(), request); !errors.Is(err, ErrToolRunNotFound) {
		t.Fatalf("lookup bypassed frozen identity validation: %v", err)
	}
}

func TestRecoverableKeySeparatesOwnershipAndKeepsInputConflictIdentity(t *testing.T) {
	service := &RunService{}
	request := validRequest()
	request.IdempotencyKey = "browser-request"
	first, _ := service.idempotencyKey(request, testInputs())
	for _, field := range []string{"project", "actor", "toolkit", "key"} {
		changed := request
		switch field {
		case "project":
			changed.ProjectID++
		case "actor":
			changed.ActorUserID++
		case "toolkit":
			changed.ToolkitID++
		case "key":
			changed.IdempotencyKey += "-next"
		}
		other, _ := service.idempotencyKey(changed, testInputs())
		if first == other {
			t.Fatalf("%s shares recovery identity", field)
		}
	}
	request.Arguments = []byte(`{"changed":true}`)
	inputs := testInputs()
	inputs.ToolkitVersion = "changed"
	other, _ := service.idempotencyKey(request, inputs)
	if first != other {
		t.Fatal("changed inputs bypass the admission digest conflict check")
	}
	for _, key := range []string{"../key", " key", "key\n", "é", "a/b"} {
		request.IdempotencyKey = key
		if request.Validate() == nil {
			t.Fatalf("accepted invalid key %q", key)
		}
	}
	if (ResultRequest{ProjectID: 1, ActorUserID: 7, ToolkitID: 19, ExecutionID: "execution", RequestKey: "key"}).Validate() == nil {
		t.Fatal("accepted two lookup identities")
	}
}
