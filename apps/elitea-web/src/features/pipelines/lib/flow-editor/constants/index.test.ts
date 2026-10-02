import { describe, expect, it } from 'vitest';

import { FlowEditorConstants, NodeDefaultsConstants, RuntimeContractConstants, StateDrawerConstants, ValidationErrors } from './index';

describe('constants barrel (unit A2c symbols)', () => {
  it('re-exports FlowEditorConstants as a namespace with real values', () => {
    expect(FlowEditorConstants.PipelineNodeTypes.Tool).toBe('tool');
  });

  it('re-exports StateDrawerConstants as a namespace with real values', () => {
    expect(StateDrawerConstants.ItemMode.Create).toBe('create');
  });

  it('re-exports ValidationErrors directly', () => {
    expect(ValidationErrors.VariableNameExists).toBe('Name already exists');
  });

  it('re-exports RuntimeContractConstants as a namespace with the compiler allow-list', () => {
    expect(RuntimeContractConstants.isCompilerAdmittedNodeType('agent')).toBe(true);
    expect(RuntimeContractConstants.isCompilerAdmittedNodeType('code')).toBe(true);
    expect(RuntimeContractConstants.isCompilerAdmittedNodeType('custom')).toBe(false);
  });

  it('re-exports NodeDefaultsConstants as a namespace with real seed data', () => {
    expect(NodeDefaultsConstants.InitialNodeData['agent']).toHaveProperty('tool');
  });
});
