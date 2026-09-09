import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapList } from '@/shared/api/unwrap';

import type {
  SkillDraft,
  SkillListPage,
  SkillRecord,
  SkillTestRequest,
  SkillWriteInput,
} from '../model/types';

interface Envelope<T> {
  readonly data: T;
  readonly headers?: Headers;
}

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<Envelope<T>>(url, options);
  return envelope.data;
}

function jsonOptions(method: 'POST' | 'PUT' | 'PATCH', body: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
}

export interface ListSkillsOptions {
  readonly page?: number;
  readonly pageSize?: number;
  readonly query?: string;
  readonly sortBy?: string;
  readonly sortOrder?: 'asc' | 'desc';
}

function buildListParams(options: ListSkillsOptions): URLSearchParams {
  const params = new URLSearchParams();
  params.set('page', String(options.page ?? 1));
  params.set('page_size', String(options.pageSize ?? 20));
  if (options.query?.trim()) params.set('query', options.query.trim());
  if (options.sortBy) params.set('sort_by', options.sortBy);
  if (options.sortOrder) params.set('sort_order', options.sortOrder);
  return params;
}

interface SkillListWire {
  readonly total?: number;
  readonly page?: number;
  readonly page_size?: number;
  readonly total_pages?: number;
}

function normaliseSkillList(wire: SkillListWire, options: ListSkillsOptions): SkillListPage {
  // The items/rows/bare-array fan-out is the one thing every list endpoint in
  // this API disagrees about; it belongs in the shared helper, not re-derived
  // here (R-A6, #132). Only the pagination metadata is this module's own.
  const items = unwrapList<SkillRecord>(wire, 'listSkills');
  const pageSize = wire.page_size ?? options.pageSize ?? 20;
  const total = wire.total ?? items.length;
  return {
    items,
    total,
    page: wire.page ?? options.page ?? 1,
    pageSize,
    totalPages: wire.total_pages ?? Math.ceil(total / pageSize),
  };
}

export async function fetchSkills(projectId: string, options: ListSkillsOptions = {}): Promise<SkillListPage> {
  const params = buildListParams(options);
  const wire = await fetchData<SkillListWire>(
    `/elitea_core/skills/prompt_lib/${projectId}?${params.toString()}`,
  );
  return normaliseSkillList(wire, options);
}

export function fetchSkill(projectId: string, skillId: string, versionId?: string): Promise<SkillRecord> {
  const suffix = versionId ? `/${versionId}` : '';
  return fetchData<SkillRecord>(`/elitea_core/skill/prompt_lib/${projectId}/${skillId}${suffix}`);
}

export function createSkill(projectId: string, input: SkillWriteInput): Promise<SkillRecord> {
  return fetchData<SkillRecord>(
    `/elitea_core/skills/prompt_lib/${projectId}`,
    jsonOptions('POST', {
      name: input.name.trim(),
      description: input.description.trim(),
      versions: [{ name: 'base', instructions: input.instructions, tags: input.tags }],
    }),
  );
}

export function updateSkill(
  projectId: string,
  skillId: string,
  input: SkillWriteInput,
  versionId?: string,
): Promise<SkillRecord> {
  const suffix = versionId ? `/${versionId}` : '';
  return fetchData<SkillRecord>(
    `/elitea_core/skill/prompt_lib/${projectId}/${skillId}${suffix}`,
    jsonOptions('PUT', {
      name: input.name.trim(),
      description: input.description.trim(),
      instructions: input.instructions,
      tags: input.tags,
    }),
  );
}

export function createSkillVersion(
  projectId: string,
  skillId: string,
  input: Pick<SkillWriteInput, 'instructions' | 'tags'> & { readonly name: string },
): Promise<SkillRecord> {
  return fetchData<SkillRecord>(
    `/elitea_core/skill/prompt_lib/${projectId}/${skillId}`,
    jsonOptions('POST', input),
  );
}

export async function deleteSkill(
  projectId: string,
  skillId: string,
  versionId?: string,
): Promise<void> {
  const suffix = versionId ? `/${versionId}` : '';
  await fetchData<unknown>(`/elitea_core/skill/prompt_lib/${projectId}/${skillId}${suffix}`, {
    method: 'DELETE',
  });
}

export function setDefaultSkillVersion(
  projectId: string,
  skillId: string,
  versionId: string | number,
): Promise<SkillRecord> {
  return fetchData<SkillRecord>(
    `/elitea_core/skill_default_version/prompt_lib/${projectId}/${skillId}`,
    jsonOptions('PATCH', { version_id: versionId }),
  );
}

/**
 * restoreSkillVersion is the rollback (#874): it copies `versionId`'s
 * instructions/tags back onto `base` and returns the skill with `base` as
 * `version_details`. Skills have no distinguished "currently active" row to
 * repoint the way agents' "Set as default" does — the unversioned GET/PUT/
 * DELETE and every fresh attachment both read `base` directly — so restoring
 * a version means overwriting the one row those actually use.
 */
export function restoreSkillVersion(
  projectId: string,
  skillId: string,
  versionId: string | number,
): Promise<SkillRecord> {
  return fetchData<SkillRecord>(
    `/elitea_core/skill_version_restore/prompt_lib/${projectId}/${skillId}/${versionId}`,
    { method: 'POST' },
  );
}

/**
 * deleteSkillVersion removes one NAMED version — a thin, explicitly-named
 * wrapper over deleteSkill(projectId, skillId, versionId) so a caller that
 * means "delete this version" does not have to remember that the whole-skill
 * delete and the version delete share one function distinguished only by an
 * optional argument.
 */
export function deleteSkillVersion(
  projectId: string,
  skillId: string,
  versionId: string,
): Promise<void> {
  return deleteSkill(projectId, skillId, versionId);
}

export function generateSkillDraft(projectId: string, description: string): Promise<SkillDraft> {
  return fetchData<SkillDraft>(
    `/elitea_core/generate_skill_draft/prompt_lib/${projectId}`,
    jsonOptions('POST', { user_description: description }),
  );
}

export function importSkill(projectId: string, file: File): Promise<SkillRecord & { readonly notice?: string }> {
  const body = new FormData();
  body.append('file', file);
  return fetchData<SkillRecord & { readonly notice?: string }>(
    `/elitea_core/skill_import/prompt_lib/${projectId}`,
    { method: 'POST', body },
  );
}

export async function exportSkill(
  projectId: string,
  skillId: string,
  versionId?: string,
): Promise<string> {
  const suffix = versionId ? `/${versionId}` : '';
  return fetchData<string>(`/elitea_core/skill_export/prompt_lib/${projectId}/${skillId}${suffix}`);
}

export function testSkill(projectId: string, request: SkillTestRequest): Promise<{ readonly task_id?: string }> {
  return fetchData<{ readonly task_id?: string }>(
    `/elitea_core/predict_llm/prompt_lib/${projectId}`,
    jsonOptions('POST', {
      sid: request.sid,
      message_id: request.messageId,
      stream_id: request.streamId,
      await_task_timeout: 0,
      instructions: request.instructions,
      user_input: request.userInput,
      chat_history: request.chatHistory,
      llm_settings: {
        model_name: request.modelName,
        ...(request.modelProjectId ? { model_project_id: request.modelProjectId } : {}),
        temperature: request.temperature,
        max_tokens: request.maxTokens,
      },
    }),
  );
}

export async function cancelSkillTest(projectId: string, taskId: string): Promise<void> {
  await fetchData<unknown>(`/elitea_core/task/prompt_lib/${projectId}/${taskId}`, { method: 'DELETE' });
}
