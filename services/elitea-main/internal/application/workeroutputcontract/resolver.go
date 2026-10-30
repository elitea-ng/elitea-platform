// Package workeroutputcontract marks an agent execution command whose frozen
// definition holds an output contract (ADR-0043 decision 2, #1500).
//
// The contract itself travels inside the frozen definition: the agent
// version's meta.output_contract, a nested agent's, or the output_contract of
// a pipeline LLM node. A worker of an earlier release reads the version meta
// loosely and would ignore a contract it cannot enforce. Main therefore sets
// WorkerCommandV1.output_contracts (field 70) on such a command: that worker
// does not know the field and refuses the command (INCOMPATIBLE_VERSION), so
// the run fails instead of returning an unchecked answer. Every other command
// is unchanged byte for byte. Argo CD deploys Main (wave 0) before the
// workers (wave 1); upgrade the workers before authors save contracts.
package workeroutputcontract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

// Revision is the contract grammar revision this Main sends (json_schema,
// regex and the json, markdown and csv formats).
const Revision = 1

// Resolver is the commandbus producers' OutputContractsResolver.
type Resolver struct {
	inputs workerdelegation.InputReader
}

// NewResolver builds the resolver over the command input reader.
func NewResolver(inputs workerdelegation.InputReader) (*Resolver, error) {
	if inputs == nil {
		return nil, errors.New("worker output contracts: the input reader is required")
	}
	return &Resolver{inputs: inputs}, nil
}

// WorkerOutputContracts answers field 70 for an agent execution command
// whose input holds an output contract, or nil. An error is an input that
// could not be read; the dispatch is retried.
func (r *Resolver) WorkerOutputContracts(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerOutputContractsV1, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if command.GetAgentExecution() == nil || bundle == "" || entryID == "" {
		return nil, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, fmt.Errorf("worker output contracts: read the command's input: %w", err)
	}
	if !InputHasContract(contents[entryID]) {
		return nil, nil
	}
	return &runtimev1.WorkerOutputContractsV1{Revision: Revision}, nil
}

// InputHasContract reports whether an agent execution input holds an output
// contract anywhere in its application: a non-null `output_contract` key at
// any depth, or a pipeline YAML string with an LLM node that declares one.
func InputHasContract(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil || len(input.GetApplication()) == 0 {
		return false
	}
	if !strings.Contains(string(input.GetApplication()), outputcontract.MetaKey) {
		return false
	}
	var application any
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
		return false
	}
	return holdsContract(application)
}

func holdsContract(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if typed[outputcontract.MetaKey] != nil {
			return true
		}
		for _, child := range typed {
			if holdsContract(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if holdsContract(child) {
				return true
			}
		}
	case string:
		return outputcontract.HasContract(nil, typed)
	}
	return false
}
