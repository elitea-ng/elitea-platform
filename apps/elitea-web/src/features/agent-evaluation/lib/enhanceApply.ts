/**
 * Turns an accepted Enhance with AI fix into the body the normal write route
 * takes. Pure, so each merge rule is tested without a server.
 */
import type {
  ApplicationVersionDetail,
  EvalDatasetCase,
  EvalEnhanceDatasetChange,
  EvalEnhanceDimensionChange,
  EvalDatasetCaseWriteRequest,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import type { EvalDimension, EvalDimensionWriteInput, EvalPolarity, EvalTargetOperator } from '../model/types';

/**
 * The dimension as the library holds it, with ONLY the proposed fields
 * replaced. The PUT is a whole-row write, so everything the proposal did not
 * touch is carried from the stored row.
 */
export function dimensionWriteInput(row: EvalDimension, change: EvalEnhanceDimensionChange): EvalDimensionWriteInput {
  const retarget = change.default_target !== undefined && change.default_target_operator !== undefined;
  return {
    name: row.name,
    description: change.description ?? row.description,
    tier: row.tier,
    application_id: row.application_id,
    allowed_engines: row.allowed_engines,
    scale_type: row.scale_type,
    scale_min: row.scale_min,
    scale_max: row.scale_max,
    polarity: row.polarity as EvalPolarity,
    default_weight: row.default_weight,
    default_target: retarget ? (change.default_target ?? null) : row.default_target,
    default_target_operator: retarget ? (change.default_target_operator as EvalTargetOperator) : row.default_target_operator,
    code: row.code,
    return_contract: row.return_contract,
  };
}

/** The case with its stored text and variables, and the proposed expected output. */
export function caseWriteInput(row: EvalDatasetCase, change: EvalEnhanceDatasetChange): EvalDatasetCaseWriteRequest {
  return { input: row.input, variables: row.variables, expected_output: change.expected_output };
}

/** A short unique-per-run name; `application_versions` is unique on (application, name). */
export function draftVersionName(runId: string): string {
  return `eval-fix-${runId}`.slice(0, 20);
}

/**
 * The pinned version's own fields with the patched instructions. Only fields
 * the write contract accepts are carried, and the result is a DRAFT: nothing
 * here publishes it. Publishing stays on the agent's release path, where a
 * medium-risk-or-above agent needs a change request and its release gate.
 */
export function draftVersionBody(base: ApplicationVersionDetail, instructions: string): Omit<VersionWriteRequest, 'name'> {
  const body: Omit<VersionWriteRequest, 'name'> = { instructions };
  if (base.agent_type !== undefined) body.agent_type = base.agent_type;
  if (base.welcome_message !== undefined) body.welcome_message = base.welcome_message;
  if (base.llm_settings !== undefined) body.llm_settings = base.llm_settings;
  if (base.conversation_starters !== undefined) body.conversation_starters = base.conversation_starters;
  if (base.variables !== undefined) body.variables = base.variables;
  if (base.tags !== undefined) body.tags = base.tags;
  if (base.meta !== undefined && base.meta !== null) body.meta = base.meta;
  if (base.pipeline_settings !== undefined) body.pipeline_settings = base.pipeline_settings;
  if (base.soul !== undefined) body.soul = base.soul;
  return body;
}
