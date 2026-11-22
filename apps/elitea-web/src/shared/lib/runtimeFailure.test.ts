import { describe, expect, it } from 'vitest';

import {
  classifyRuntimeFailure,
  delegationRefusalExplainsFailure,
  isTypedRefusalCode,
  refusalCategoryTitle,
  refusalFallbackMessage,
} from './runtimeFailure';

describe('classifyRuntimeFailure', () => {
  it.each([
    ['AUTHORIZATION_FAILED', 'authorization'],
    ['MODEL_ACCESS_DENIED', 'authorization'],
    ['CAP_REACHED', 'policy'],
    ['MODEL_BUDGET_EXHAUSTED', 'policy'],
    ['PROJECT_BUDGET_EXHAUSTED', 'policy'],
    ['MEMBER_BUDGET_EXHAUSTED', 'policy'],
    ['MODEL_REQUEST_REJECTED', 'policy'],
    ['RESULT_REFUSED', 'policy'],
    ['RECOVERY_REFUSED', 'policy'],
    ['RESOURCE_EXHAUSTED', 'policy'],
    ['INVALID_INPUT', 'validation'],
    ['UNSUPPORTED_CAPABILITY', 'validation'],
    ['INCOMPATIBLE_VERSION', 'validation'],
    ['EXECUTION_INPUT_LIMIT', 'validation'],
    ['CONTEXT_BUDGET_EXCEEDED', 'validation'],
    ['MODEL_REQUEST_TOO_LARGE', 'validation'],
    ['PIPELINE_INPUT_INVALID', 'validation'],
    ['PIPELINE_INPUT_LIMIT', 'validation'],
    ['PIPELINE_RESULT_INVALID', 'validation'],
    ['PIPELINE_RESULT_LIMIT', 'validation'],
    ['OUTPUT_DELIVERY_LIMIT', 'validation'],
    ['RECOVERY_FRONTIER_UNSUPPORTED', 'validation'],
    ['PIPELINE_TOOL_UNAVAILABLE', 'validation'],
  ] as const)('classifies %s as a %s refusal', (code, category) => {
    expect(classifyRuntimeFailure(code)).toEqual({ kind: 'refusal', category });
    expect(isTypedRefusalCode(code)).toBe(true);
  });

  it('classifies CANCELLED as cancelled', () => {
    expect(classifyRuntimeFailure('CANCELLED')).toEqual({ kind: 'cancelled' });
    expect(isTypedRefusalCode('CANCELLED')).toBe(false);
  });

  it.each([
    'INTERNAL',
    'DEPENDENCY_UNAVAILABLE',
    'DEADLINE_EXCEEDED',
    'MODEL_TIMEOUT',
    'MODEL_RATE_LIMITED',
    'MODEL_UNAVAILABLE',
    'MODEL_PROVIDER_FAILURE',
    'MODEL_RESPONSE_INVALID',
    'PIPELINE_TOOL_FAILED',
    'PIPELINE_CODE_FAILED',
    'OUTPUT_CONTINUATION_EXHAUSTED',
    'UNKNOWN_CODE',
    '',
    undefined,
  ])('keeps %s as a runtime error', (code) => {
    expect(classifyRuntimeFailure(code)).toEqual({ kind: 'error' });
    expect(isTypedRefusalCode(code)).toBe(false);
  });
});

describe('delegationRefusalExplainsFailure', () => {
  it('returns true only when AUTHORIZATION_FAILED has a fatal pipeline_node refused notice and a generic message', () => {
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'pipeline_node' }])).toBe(true);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'pipeline_node' }], 'Execution authorization failed.')).toBe(true);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'pipeline_node' }], 'AUTHORIZATION_FAILED')).toBe(true);
    // SEC-FU359-01: non-fatal tool/child notices never suppress a fatal AUTHORIZATION_FAILED banner.
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'agent_tool' }])).toBe(false);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'pipeline_child' }])).toBe(false);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'sub_agent' }])).toBe(false);
    // Distinct non-generic authorization failure sentences are never suppressed.
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'refused', edgeKind: 'pipeline_node' }], 'Code node authorization failed.')).toBe(false);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [{ kind: 'narrowed', edgeKind: 'pipeline_node' }])).toBe(false);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', [])).toBe(false);
    expect(delegationRefusalExplainsFailure('AUTHORIZATION_FAILED', undefined)).toBe(false);
    expect(delegationRefusalExplainsFailure('INTERNAL', [{ kind: 'refused', edgeKind: 'pipeline_node' }])).toBe(false);
  });
});

describe('refusal labels and fallbacks', () => {
  it('returns sentence-case headings and calm fallbacks per category', () => {
    expect(refusalCategoryTitle('authorization')).toBe('Authorization refused');
    expect(refusalCategoryTitle('policy')).toBe('Request refused by policy');
    expect(refusalCategoryTitle('validation')).toBe('Request refused');
    expect(refusalFallbackMessage('authorization')).toContain('authorization');
    expect(refusalFallbackMessage('policy')).toContain('policy');
    expect(refusalFallbackMessage('validation')).toContain('request or configuration');
  });
});
