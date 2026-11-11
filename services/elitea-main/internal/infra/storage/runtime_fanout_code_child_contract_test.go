package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// R9: Main's fan-out Code child bound is the shared contract's max_children
// (libs/proto/contracts/pipeline-fanout-child-families-v1.json), which the
// Worker (MAX_FANOUT_CHILDREN) and the Web editor (MAP_MAX_ITEMS) assert too.
func TestTheFanoutChildBoundIsTheContractBound(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/pipeline-fanout-child-families-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		MaxChildren int `json:"max_children"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.MaxChildren != maxFanoutChildren {
		t.Fatalf("contract max_children %d, Main %d", contract.MaxChildren, maxFanoutChildren)
	}
}
