package commandbus

// The worker egress policy (ADR-0044 decision 2) on every producer that
// prepares a WorkerCommandV1: each wraps the one Producer whose
// prepareCommand assembles it, so these only reach it.

// WithEgressPolicy makes the agent execution and direct toolkit read commands
// carry their policy.
func (p *AgentExecutionProducer) WithEgressPolicy(resolver EgressPolicyResolver) *AgentExecutionProducer {
	p.producer.WithEgressPolicy(resolver)
	return p
}

// WithDelegationPolicy makes the agent execution commands carry their
// delegation policy (ADR-0044 decision 3).
func (p *AgentExecutionProducer) WithDelegationPolicy(resolver DelegationPolicyResolver) *AgentExecutionProducer {
	p.producer.WithDelegationPolicy(resolver)
	return p
}

// WithApprovalPolicy makes the agent execution commands carry their approval
// policy (ADR-0041 decision 5).
func (p *AgentExecutionProducer) WithApprovalPolicy(resolver ApprovalPolicyResolver) *AgentExecutionProducer {
	p.producer.WithApprovalPolicy(resolver)
	return p
}

// WithRunCaps makes the agent execution commands carry their run caps
// (ADR-0041 decision 3).
func (p *AgentExecutionProducer) WithRunCaps(resolver RunCapsResolver) *AgentExecutionProducer {
	p.producer.WithRunCaps(resolver)
	return p
}

// WithOutputContracts makes the agent execution commands whose definition
// holds an output contract carry field 70 (ADR-0043 decision 2).
func (p *AgentExecutionProducer) WithOutputContracts(resolver OutputContractsResolver) *AgentExecutionProducer {
	p.producer.WithOutputContracts(resolver)
	return p
}

// WithEgressPolicy makes the index ingest commands carry their policy.
func (p *IndexIngestProducer) WithEgressPolicy(resolver EgressPolicyResolver) *IndexIngestProducer {
	p.producer.WithEgressPolicy(resolver)
	return p
}

// WithEgressPolicy makes the tool-call commands carry their policy.
func (p *ToolkitCallToolProducer) WithEgressPolicy(resolver EgressPolicyResolver) *ToolkitCallToolProducer {
	p.producer.WithEgressPolicy(resolver)
	return p
}

// WithEgressPolicy makes the tool discovery commands carry their policy.
func (p *ToolkitAvailableToolsProducer) WithEgressPolicy(resolver EgressPolicyResolver) *ToolkitAvailableToolsProducer {
	p.producer.WithEgressPolicy(resolver)
	return p
}

// WithDispatchRefusals records a refused policy so the run fails at once.
func (p *AgentExecutionProducer) WithDispatchRefusals(recorder DispatchRefusalRecorder) *AgentExecutionProducer {
	p.producer.WithDispatchRefusals(recorder)
	return p
}

// WithDispatchRefusals records a refused policy so the run fails at once.
func (p *IndexIngestProducer) WithDispatchRefusals(recorder DispatchRefusalRecorder) *IndexIngestProducer {
	p.producer.WithDispatchRefusals(recorder)
	return p
}

// WithDispatchRefusals records a refused policy so the run fails at once.
func (p *ToolkitCallToolProducer) WithDispatchRefusals(recorder DispatchRefusalRecorder) *ToolkitCallToolProducer {
	p.producer.WithDispatchRefusals(recorder)
	return p
}

// WithDispatchRefusals records a refused policy so the run fails at once.
func (p *ToolkitAvailableToolsProducer) WithDispatchRefusals(recorder DispatchRefusalRecorder) *ToolkitAvailableToolsProducer {
	p.producer.WithDispatchRefusals(recorder)
	return p
}
