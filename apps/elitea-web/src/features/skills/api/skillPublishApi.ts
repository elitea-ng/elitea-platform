/**
 * The skill-publishing wire: publish, unpublish, the pre-publish gate, the
 * category list, the public catalog and the attach-to-agent fork.
 *
 * ## Why this uses the GENERATED client while its neighbour does not
 *
 * `skillsApi.ts` next door builds its own `eliteaFetch` calls, because the
 * skill CRUD routes it talks to are not in the OpenAPI spec. These nine are
 * (`services/elitea-main/api/openapi/v2.yaml`, tag `skills`), so the generated
 * functions exist and carry the request/response shapes the server actually
 * declares. Hand-rolling a second copy of a URL the generator already emits is
 * how a client drifts from a contract that is right there.
 *
 * What this module adds on top is the envelope unwrap and the error shaping.
 * `eliteaFetch` resolves with `{data, status}` and REJECTS with an
 * `EliteaApiError` on any 4xx/5xx, and both of those matter here:
 *
 *  - a caller that forgets `.data` renders an empty screen against a correct
 *    API, which is a defect this codebase has shipped before;
 *  - the pre-publish gate answers **422 with a body** on FAIL. That is a
 *    rejection, not a result, so the validation report would be thrown away by
 *    an ordinary `catch` — `validateSkillForPublish` below reads it back out of
 *    the failure and returns it, because a FAIL report is the whole point of
 *    calling the gate.
 */
import {
  attachPublicSkill as attachPublicSkillRequest,
  listAgentsWithSkill as listAgentsWithSkillRequest,
  listPublicSkills as listPublicSkillsRequest,
  listSkillCategories as listSkillCategoriesRequest,
  publishSkill as publishSkillRequest,
  unpublishSkill as unpublishSkillRequest,
  validateSkillForPublish as validateSkillForPublishRequest,
} from '@/shared/api/generated/skills/skills';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import type {
  AgentWithSkill,
  AttachOutcome,
  PublicSkillListPage,
  PublicSkillSummary,
  SkillCategory,
  SkillValidationReport,
} from '../model/publishTypes';

/** The `{data, status}` envelope every generated operation resolves with. */
interface Envelope<T> {
  readonly data?: T;
}

function unwrap<T>(envelope: unknown): T | undefined {
  return (envelope as Envelope<T> | undefined)?.data;
}

/**
 * The body of an HTTP failure, when there is one.
 *
 * Everything that reaches a user as a message goes through here rather than
 * through `error.message`, which is the transport's own string
 * (`eliteaFetch: 403 from …`) and says nothing about what the platform
 * refused.
 */
function failureBody(error: unknown): Record<string, unknown> | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const body = error.failure.body;
  return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : undefined;
}

function failureStatus(error: unknown): number | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  return error.failure.kind === 'http' || error.failure.kind === 'auth' ? error.failure.status : undefined;
}

/**
 * The server's own sentence for a refusal.
 *
 * elitea-main answers publishing refusals as `{error, msg}` and validation
 * errors as `{error: [{loc, msg}]}` (FastAPI's shape, kept for drop-in
 * compatibility), so both are read. The fallback is the caller's, never a
 * transport string.
 */
export function publishErrorMessage(error: unknown, fallback: string): string {
  const body = failureBody(error);
  if (!body) return fallback;
  if (typeof body.msg === 'string' && body.msg) return body.msg;
  if (Array.isArray(body.error)) {
    const first = body.error[0] as { readonly msg?: unknown } | undefined;
    if (typeof first?.msg === 'string' && first.msg) return first.msg;
  }
  if (typeof body.error === 'string' && body.error) return body.error;
  return fallback;
}

export interface PublishSkillInput {
  readonly versionName: string;
  readonly category?: string;
  readonly validationToken?: string;
}

export async function publishSkill(
  projectId: string,
  skillId: number,
  versionId: number,
  input: PublishSkillInput,
): Promise<Record<string, unknown>> {
  const response = await publishSkillRequest(projectId, skillId, versionId, {
    version_name: input.versionName,
    ...(input.category ? { category: input.category } : {}),
    ...(input.validationToken ? { validation_token: input.validationToken } : {}),
  });
  return unwrap<Record<string, unknown>>(response) ?? {};
}

export async function unpublishSkill(
  projectId: string,
  skillId: number,
  versionId: number,
): Promise<void> {
  await unpublishSkillRequest(projectId, skillId, versionId, {});
}

/**
 * Runs the pre-publish gate and returns its report whether it passed or failed.
 *
 * A FAIL arrives as a 422, which `eliteaFetch` turns into a rejection. The
 * report is in that rejection's body, so it is read back rather than rethrown:
 * to the caller, "the gate ran and said no" is an ANSWER, and only a transport
 * or authorisation failure is an error.
 */
export async function validateSkillForPublish(
  projectId: string,
  skillId: number,
  versionId: number,
  input: { readonly versionName: string; readonly category?: string },
): Promise<SkillValidationReport> {
  try {
    const response = await validateSkillForPublishRequest(projectId, skillId, versionId, {
      version_name: input.versionName,
      ...(input.category ? { category: input.category } : {}),
    });
    return unwrap<SkillValidationReport>(response) ?? emptyReport();
  } catch (error) {
    const body = failureBody(error);
    if (failureStatus(error) === 422 && body && typeof body.status === 'string') {
      return body as unknown as SkillValidationReport;
    }
    throw error;
  }
}

function emptyReport(): SkillValidationReport {
  return {
    status: 'PASS',
    critical_issues: [],
    warnings: [],
    recommendations: [],
    summary: '',
    ai_validation_available: false,
  };
}

export async function fetchSkillCategories(projectId: string): Promise<readonly SkillCategory[]> {
  const response = await listSkillCategoriesRequest(projectId);
  return unwrap<{ readonly categories?: readonly SkillCategory[] }>(response)?.categories ?? [];
}

export interface PublicSkillQuery {
  readonly query?: string;
  readonly category?: string;
  readonly limit?: number;
  readonly offset?: number;
}

export async function fetchPublicSkills(request: PublicSkillQuery = {}): Promise<PublicSkillListPage> {
  const response = await listPublicSkillsRequest({
    ...(request.query ? { query: request.query } : {}),
    ...(request.category ? { category: request.category } : {}),
    ...(request.limit === undefined ? {} : { limit: request.limit }),
    ...(request.offset === undefined ? {} : { offset: request.offset }),
  });
  const page = unwrap<{ readonly rows?: readonly PublicSkillSummary[]; readonly total?: number }>(response);
  const rows = page?.rows ?? [];
  return { rows, total: page?.total ?? rows.length };
}

/**
 * Agent versions in this project that already carry the public skill, through
 * the fork lineage the server walks (`internal/api/v2/skillpublish/attach.go`
 * `AgentsWithSkill`). The attach dialog reads this so it never offers a
 * second attach to a version that has the skill already — the write route
 * answers that with a 409, not a fabricated success.
 */
export async function fetchAgentsWithSkill(
  projectId: string,
  publicSkillId: number | undefined,
): Promise<readonly AgentWithSkill[]> {
  if (projectId === '' || publicSkillId === undefined) return [];
  const response = await listAgentsWithSkillRequest(projectId, publicSkillId);
  return unwrap<{ readonly rows?: readonly AgentWithSkill[] }>(response)?.rows ?? [];
}

export async function attachPublicSkill(
  projectId: string,
  input: {
    readonly publicSkillId: number;
    readonly publicVersionId: number;
    readonly agentVersionIds: readonly number[];
  },
): Promise<readonly AttachOutcome[]> {
  const response = await attachPublicSkillRequest(projectId, {
    public_skill_id: input.publicSkillId,
    public_version_id: input.publicVersionId,
    agent_version_ids: [...input.agentVersionIds],
    entity_type: 'agent',
  });
  return unwrap<{ readonly results?: readonly AttachOutcome[] }>(response)?.results ?? [];
}
