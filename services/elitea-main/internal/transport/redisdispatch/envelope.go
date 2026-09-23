package redisdispatch

import (
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const redisEnvelopeField = "signed_envelope"

func validationWorkerCommand(protocolRevision string, dispatch executionapp.ValidationDispatch) (*runtimev1.WorkerCommandV1, error) {
	if protocolRevision == "" || len(protocolRevision) > 128 {
		return nil, errors.New("invalid protocol revision")
	}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}

	command := dispatch.Command
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    protocolRevision,
		CommandId:           dispatch.CommandID,
		IdempotencyKey:      dispatch.OutboxID,
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE,
		ExecutionId:         dispatch.ExecutionID,
		Generation:          dispatch.Generation,
		DispatchOrdinal:     dispatch.DispatchOrdinal,
		RootExecutionId:     dispatch.ExecutionID,
		TenantId:            dispatch.TenantID,
		ResourceProjectId:   dispatch.ResourceProjectID,
		ProjectionProjectId: dispatch.ProjectionProjectID,
		PrincipalRef:        dispatch.PrincipalRef,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    dispatch.InputBundleID,
			ImmutableVersion: dispatch.InputBundleVersion,
			Digest:           digestProto(dispatch.InputBundleDigest),
			ByteLength:       dispatch.InputBundleByteLength,
			MediaType:        dispatch.InputBundleMediaType,
		},
		CapabilityId:       executiondomain.ConfigurationValidationCapability,
		CapabilityVersion:  dispatch.CapabilityVersion,
		ResourceClass:      dispatch.ResourceClass,
		IsolationClass:     dispatch.IsolationClass,
		Priority:           dispatch.Priority,
		DeadlineUnixMillis: dispatch.Deadline.UTC().UnixMilli(),
		Traceparent:        dispatch.Traceparent,
		Tracestate:         dispatch.Tracestate,
		LimitsRevision:     dispatch.LimitsRevision,
		CapabilityCommand: &runtimev1.WorkerCommandV1_ConfigurationValidation{
			ConfigurationValidation: &runtimev1.ConfigurationValidationCommandV1{
				ConfigurationRevisionId: command.ConfigurationRevisionID,
				ConfigurationType:       command.ConfigurationType,
				CatalogRevision:         command.CatalogRevision,
				CatalogDigest:           digestProto(command.CatalogDigest),
				SchemaId:                command.SchemaID,
				SchemaRevision:          command.SchemaRevision,
				SchemaDigest:            digestProto(command.SchemaDigest),
				SettingsEntryId:         command.SettingsEntryID,
			},
		},
	}, nil
}

func agentExecutionWorkerCommand(protocolRevision string, dispatch agentexecutionapp.AgentExecutionDispatch) (*runtimev1.WorkerCommandV1, error) {
	if protocolRevision == "" || len(protocolRevision) > 128 {
		return nil, errors.New("invalid protocol revision")
	}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}
	commandType := runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION
	if dispatch.CapabilityID == executiondomain.AgentAdhocCapability {
		commandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
	}
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    protocolRevision,
		CommandId:           dispatch.CommandID,
		IdempotencyKey:      dispatch.OutboxID,
		CommandType:         commandType,
		ExecutionId:         dispatch.ExecutionID,
		Generation:          dispatch.Generation,
		DispatchOrdinal:     dispatch.DispatchOrdinal,
		RootExecutionId:     dispatch.ExecutionID,
		TenantId:            dispatch.TenantID,
		ResourceProjectId:   dispatch.ResourceProjectID,
		ProjectionProjectId: dispatch.ProjectionProjectID,
		PrincipalRef:        dispatch.PrincipalRef,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    dispatch.InputBundleID,
			ImmutableVersion: dispatch.InputBundleVersion,
			Digest:           digestProto(dispatch.InputBundleDigest),
			ByteLength:       dispatch.InputBundleByteLength,
			MediaType:        dispatch.InputBundleMediaType,
		},
		CapabilityId:       dispatch.CapabilityID,
		CapabilityVersion:  dispatch.CapabilityVersion,
		ResourceClass:      dispatch.ResourceClass,
		IsolationClass:     dispatch.IsolationClass,
		Priority:           dispatch.Priority,
		DeadlineUnixMillis: dispatch.Deadline.UTC().UnixMilli(),
		Traceparent:        dispatch.Traceparent,
		Tracestate:         dispatch.Tracestate,
		LimitsRevision:     dispatch.LimitsRevision,
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{
				RequestEntryId:  dispatch.RequestEntryID,
				ClientStreamId:  dispatch.ClientStreamID,
				ClientMessageId: dispatch.ClientMessageID,
				SioEvent:        dispatch.SIOEvent,
			},
		},
	}, nil
}

func toolkitExecuteReadWorkerCommand(
	protocolRevision string,
	dispatch toolkitexecutionapp.ToolkitExecuteReadDispatch,
) (*runtimev1.WorkerCommandV1, error) {
	if protocolRevision == "" || len(protocolRevision) > 128 {
		return nil, errors.New("invalid protocol revision")
	}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    protocolRevision,
		CommandId:           dispatch.CommandID,
		IdempotencyKey:      dispatch.OutboxID,
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_EXECUTE_READ,
		ExecutionId:         dispatch.ExecutionID,
		Generation:          dispatch.Generation,
		DispatchOrdinal:     dispatch.DispatchOrdinal,
		RootExecutionId:     dispatch.ExecutionID,
		TenantId:            dispatch.TenantID,
		ResourceProjectId:   dispatch.ResourceProjectID,
		ProjectionProjectId: dispatch.ProjectionProjectID,
		PrincipalRef:        dispatch.PrincipalRef,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    dispatch.InputBundleID,
			ImmutableVersion: dispatch.InputBundleVersion,
			Digest:           digestProto(dispatch.InputBundleDigest),
			ByteLength:       dispatch.InputBundleByteLength,
			MediaType:        dispatch.InputBundleMediaType,
		},
		CapabilityId:       dispatch.CapabilityID,
		CapabilityVersion:  dispatch.CapabilityVersion,
		ResourceClass:      dispatch.ResourceClass,
		IsolationClass:     dispatch.IsolationClass,
		Priority:           dispatch.Priority,
		DeadlineUnixMillis: dispatch.Deadline.UTC().UnixMilli(),
		Traceparent:        dispatch.Traceparent,
		Tracestate:         dispatch.Tracestate,
		LimitsRevision:     dispatch.LimitsRevision,
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitExecuteRead{
			ToolkitExecuteRead: &runtimev1.ToolkitExecuteReadCommandV1{
				RequestEntryId: dispatch.RequestEntryID,
			},
		},
	}, nil
}

func indexIngestWorkerCommand(protocolRevision string, dispatch indexingapp.IndexIngestDispatch) (*runtimev1.WorkerCommandV1, error) {
	if protocolRevision == "" || len(protocolRevision) > 128 {
		return nil, errors.New("invalid protocol revision")
	}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}
	indexCommand := &runtimev1.IndexIngestCommandV1{
		ToolkitConfigurationEntryId: dispatch.ToolkitConfigurationEntryID,
		ToolParametersEntryId:       dispatch.ToolParametersEntryID,
		LlmModelEntryId:             dispatch.LLMModelEntryID,
		LlmConfigurationEntryId:     dispatch.LLMConfigurationEntryID,
		McpTokensEntryId:            dispatch.MCPTokensEntryID,
		ClientStreamId:              dispatch.ClientStreamID,
		ClientMessageId:             dispatch.ClientMessageID,
		SioEvent:                    dispatch.SIOEvent,
		Initiator:                   string(dispatch.Initiator),
	}
	if dispatch.EmbeddingBindingEntryID != "" {
		indexCommand.EmbeddingBinding = &runtimev1.IndexIngestInputBindingV1{
			EntryId:          dispatch.EmbeddingBindingEntryID,
			ImmutableVersion: dispatch.EmbeddingBindingDigest.String(),
			ContentDigest:    digestProto(dispatch.EmbeddingBindingDigest),
		}
	}
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    protocolRevision,
		CommandId:           dispatch.CommandID,
		IdempotencyKey:      dispatch.OutboxID,
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST,
		ExecutionId:         dispatch.ExecutionID,
		Generation:          dispatch.Generation,
		DispatchOrdinal:     dispatch.DispatchOrdinal,
		RootExecutionId:     dispatch.ExecutionID,
		TenantId:            dispatch.TenantID,
		ResourceProjectId:   dispatch.ResourceProjectID,
		ProjectionProjectId: dispatch.ProjectionProjectID,
		PrincipalRef:        dispatch.PrincipalRef,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    dispatch.InputBundleID,
			ImmutableVersion: dispatch.InputBundleVersion,
			Digest:           digestProto(dispatch.InputBundleDigest),
			ByteLength:       dispatch.InputBundleByteLength,
			MediaType:        dispatch.InputBundleMediaType,
		},
		CapabilityId:       executiondomain.IndexIngestCapability,
		CapabilityVersion:  dispatch.CapabilityVersion,
		ResourceClass:      dispatch.ResourceClass,
		IsolationClass:     dispatch.IsolationClass,
		Priority:           dispatch.Priority,
		DeadlineUnixMillis: dispatch.Deadline.UTC().UnixMilli(),
		Traceparent:        dispatch.Traceparent,
		Tracestate:         dispatch.Tracestate,
		LimitsRevision:     dispatch.LimitsRevision,
		CapabilityCommand: &runtimev1.WorkerCommandV1_IndexIngest{
			IndexIngest: indexCommand,
		},
	}, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}

func validateBoundedStrings(command *runtimev1.WorkerCommandV1, maximum int) error {
	values := []string{
		command.GetProtocolRevision(), command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(),
		command.GetRootExecutionId(), command.GetParentExecutionId(), command.GetParentCallId(), command.GetTenantId(),
		command.GetResourceProjectId(), command.GetProjectionProjectId(), command.GetPrincipalRef(),
		command.GetCapabilityId(), command.GetCapabilityVersion(), command.GetResourceClass(), command.GetIsolationClass(),
		command.GetTraceparent(), command.GetTracestate(), command.GetLimitsRevision(),
	}
	if ref := command.GetInputBundleRef(); ref != nil {
		values = append(values, ref.GetInputBundleId(), ref.GetImmutableVersion(), ref.GetMediaType())
	}
	if validation := command.GetConfigurationValidation(); validation != nil {
		values = append(values,
			validation.GetConfigurationRevisionId(), validation.GetConfigurationType(), validation.GetCatalogRevision(),
			validation.GetSchemaId(), validation.GetSchemaRevision(), validation.GetSettingsEntryId(),
		)
	}
	if index := command.GetIndexIngest(); index != nil {
		values = append(values,
			index.GetToolkitConfigurationEntryId(), index.GetToolParametersEntryId(), index.GetLlmModelEntryId(),
			index.GetLlmConfigurationEntryId(), index.GetMcpTokensEntryId(), index.GetClientStreamId(),
			index.GetClientMessageId(), index.GetSioEvent(), index.GetInitiator(),
		)
		if binding := index.GetEmbeddingBinding(); binding != nil {
			values = append(values, binding.GetEntryId(), binding.GetImmutableVersion())
		}
	}
	if agent := command.GetAgentExecution(); agent != nil {
		values = append(values,
			agent.GetRequestEntryId(), agent.GetClientStreamId(),
			agent.GetClientMessageId(), agent.GetSioEvent(),
		)
	}
	for _, value := range values {
		if len(value) > maximum || strings.ContainsRune(value, '\x00') {
			return ErrControlMessageLimitExceeded
		}
	}
	return nil
}
