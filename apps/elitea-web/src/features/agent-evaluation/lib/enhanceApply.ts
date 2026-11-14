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

/**
 * A name unique per accept: `application_versions` is unique on
 * (application, name), and a second accept of the same run (a retry, a second
 * review) must not collide with the first draft.
 */
export function draftVersionName(runId: string, now: number = Date.now()): string {
  return `eval-fix-${runId}-${now.toString(36)}`;
}

/** Keys the server owns on a stored version: never sent back on a create. */
const SERVER_OWNED_KEYS = [
  'id',
  'application_id',
  'name',
  'status',
  'created_at',
  'author_id',
  'author',
  // Toolkit and tool bindings are copied by the server (`copy_tools_from_version_id`), not re-sent.
  'tools',
  'is_forked',
] as const;

/**
 * The pinned version, field for field, with the patched instructions: every
 * stored field the write contract accepts is carried (model settings, welcome
 * message, conversation starters, variables, tags, meta, pipeline settings,
 * soul, avatar pack and anything added later), and ONLY the instructions
 * differ. Skills and tool bindings come across by the server's copy-source
 * options. The result is a DRAFT: nothing here publishes it. Publishing stays
 * on the agent's release path, where a medium-risk-or-above agent needs a
 * change request and its release gate.
 */
export function draftVersionBody(base: ApplicationVersionDetail, instructions: string): Omit<VersionWriteRequest, 'name'> {
  const copy: Record<string, unknown> = { ...base };
  for (const key of SERVER_OWNED_KEYS) delete copy[key];
  for (const [key, value] of Object.entries(copy)) {
    // An absent or null field is "nothing stored": leave it off, so the new version stores the default.
    if (value === null || value === undefined) delete copy[key];
  }
  return { ...copy, instructions };
}
