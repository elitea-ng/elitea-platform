/**
 * Classification of typed runtime failures into intentional refusals
 * (delegation, authorization, policy, validation/configuration limits),
 * user/operator cancellations, and real runtime errors.
 *
 * Shared across chat messages, workspace cloud transcripts, and pipeline run
 * indicators so live and reloaded views distinguish a typed refusal from a
 * generic runtime failure consistently.
 */
import type { DelegationNotice } from '@elitea/client-core/stream';

import { t } from '@/shared/i18n';

export type RefusalCategory = 'authorization' | 'policy' | 'validation';

export type RuntimeFailureClassification =
  | { readonly kind: 'refusal'; readonly category: RefusalCategory }
  | { readonly kind: 'cancelled' }
  | { readonly kind: 'error' };

const AUTHORIZATION_REFUSAL_CODES: ReadonlySet<string> = new Set([
  'AUTHORIZATION_FAILED',
  'MODEL_ACCESS_DENIED',
]);

const POLICY_REFUSAL_CODES: ReadonlySet<string> = new Set([
  'CAP_REACHED',
  'MODEL_BUDGET_EXHAUSTED',
  'PROJECT_BUDGET_EXHAUSTED',
  'MEMBER_BUDGET_EXHAUSTED',
  'MODEL_REQUEST_REJECTED',
  'RESULT_REFUSED',
  'RECOVERY_REFUSED',
  'RESOURCE_EXHAUSTED',
]);

const VALIDATION_REFUSAL_CODES: ReadonlySet<string> = new Set([
  'INVALID_INPUT',
  'UNSUPPORTED_CAPABILITY',
  'INCOMPATIBLE_VERSION',
  'EXECUTION_INPUT_LIMIT',
  'CONTEXT_BUDGET_EXCEEDED',
  'MODEL_REQUEST_TOO_LARGE',
  'PIPELINE_INPUT_INVALID',
  'PIPELINE_INPUT_LIMIT',
  'PIPELINE_RESULT_INVALID',
  'PIPELINE_RESULT_LIMIT',
  'OUTPUT_DELIVERY_LIMIT',
  'RECOVERY_FRONTIER_UNSUPPORTED',
  'PIPELINE_TOOL_UNAVAILABLE',
]);

/** Classify a server `failureCode` / `meta.error_code`. Unknown or missing codes are errors. */
export function classifyRuntimeFailure(code: string | undefined): RuntimeFailureClassification {
  if (code === undefined || code === '') return { kind: 'error' };
  if (code === 'CANCELLED') return { kind: 'cancelled' };
  if (AUTHORIZATION_REFUSAL_CODES.has(code)) return { kind: 'refusal', category: 'authorization' };
  if (POLICY_REFUSAL_CODES.has(code)) return { kind: 'refusal', category: 'policy' };
  if (VALIDATION_REFUSAL_CODES.has(code)) return { kind: 'refusal', category: 'validation' };
  return { kind: 'error' };
}

/** True when `code` names a typed authorization, policy, or validation refusal. */
export function isTypedRefusalCode(code: string | undefined): boolean {
  return classifyRuntimeFailure(code).kind === 'refusal';
}

const GENERIC_AUTH_FAILURE_MESSAGES: ReadonlySet<string> = new Set([
  '',
  'AUTHORIZATION_FAILED',
  'Execution authorization failed.',
]);

/**
 * True when an `AUTHORIZATION_FAILED` turn already carries a fatal graph-node
 * (`edgeKind === 'pipeline_node'`) `delegation_refused` notice (live or
 * persisted in `meta.delegation_notices_v1`), and the failure carries no
 * distinct non-generic authorization message. Non-fatal tool/sub-agent refusal
 * notices (`agent_tool`, `pipeline_child`, `sub_agent`) never suppress a
 * subsequent fatal `AUTHORIZATION_FAILED` error banner.
 */
export function delegationRefusalExplainsFailure(
  failureCode: string | undefined,
  notices: readonly Pick<DelegationNotice, 'kind' | 'edgeKind'>[] | undefined,
  exception?: unknown,
): boolean {
  if (failureCode !== 'AUTHORIZATION_FAILED' || notices === undefined) return false;
  if (exception !== undefined && (typeof exception !== 'string' || !GENERIC_AUTH_FAILURE_MESSAGES.has(exception.trim()))) {
    return false;
  }
  return notices.some((notice) => notice.kind === 'refused' && notice.edgeKind === 'pipeline_node');
}

/** Sentence-case status heading for a typed refusal. */
export function refusalCategoryTitle(category: RefusalCategory): string {
  switch (category) {
    case 'authorization':
      return t('runtimeFailure.refusal.authorizationTitle', 'Authorization refused');
    case 'policy':
      return t('runtimeFailure.refusal.policyTitle', 'Request refused by policy');
    case 'validation':
      return t('runtimeFailure.refusal.validationTitle', 'Request refused');
  }
}

/** Readable fallback reason when a typed refusal arrives with only a raw error code and no safe message. */
export function refusalFallbackMessage(category: RefusalCategory): string {
  switch (category) {
    case 'authorization':
      return t('runtimeFailure.refusal.authorizationFallback', 'This run was refused because authorization failed.');
    case 'policy':
      return t('runtimeFailure.refusal.policyFallback', 'This run was refused by a platform or budget policy.');
    case 'validation':
      return t('runtimeFailure.refusal.validationFallback', 'This run was refused because the request or configuration could not be accepted.');
  }
}
