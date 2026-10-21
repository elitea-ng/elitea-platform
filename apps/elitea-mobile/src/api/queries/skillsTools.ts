import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { queryKeys } from '@/api/queries/keys';

/**
 * What "~" and "/" offer (client contract 1.1, read only): the skills
 * attached to the addressed agent's version (listApplicationSkills) and the
 * tools of a toolkit in a model chat (listToolkitAvailableTools). A 404, 403,
 * 422 or 501 reads as "not available" (an older server, or discovery off).
 */

export interface SkillOption {
  name: string;
  description: string;
}

export interface ToolOption {
  name: string;
  description: string;
}

const UNAVAILABLE = new Set([403, 404, 422, 501, 503]);
const text = (v: unknown, max = 300): string => (typeof v === 'string' ? v.trim().slice(0, max) : '');

export async function fetchAgentSkills(
  projectId: number,
  versionId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<SkillOption[] | null> {
  const { data, response } = await client.GET('/elitea_core/application_skills/prompt_lib/{project_id}/{app_version_id}', {
    params: { path: { project_id: String(projectId), app_version_id: versionId } },
    signal,
  });
  if (!response.ok) {
    if (UNAVAILABLE.has(response.status)) return null;
    throw new Error(`skills: HTTP ${response.status}`);
  }
  // The reviewed route carries both `skills` (attachments) and `items` (skills); read the union.
  const attached = Array.isArray(data?.skills) ? data.skills : [];
  const listed = Array.isArray(data?.items) ? data.items : [];
  const out: SkillOption[] = [];
  for (const row of [...attached, ...listed]) {
    const name = text((row as { name?: unknown })?.name, 200);
    if (name && !out.some((s) => s.name === name)) out.push({ name, description: text((row as { description?: unknown }).description) });
  }
  return out;
}

export async function fetchToolkitTools(
  projectId: number,
  toolkitId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ToolOption[] | null> {
  const { data, response } = await client.GET('/elitea_core/toolkit_available_tools/prompt_lib/{project_id}/{toolkit_id}', {
    params: { path: { project_id: String(projectId), toolkit_id: toolkitId } },
    signal,
  });
  if (!response.ok) {
    if (UNAVAILABLE.has(response.status)) return null;
    throw new Error(`toolkit tools: HTTP ${response.status}`);
  }
  return (Array.isArray(data?.tools) ? data.tools : [])
    .map((t) => ({ name: text(t?.name, 200), description: text(t?.description) }))
    .filter((t) => t.name !== '');
}

export function useAgentSkills(projectId: number | null, versionId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'skills', versionId ?? 0],
    enabled: enabled && projectId !== null && versionId !== null,
    staleTime: 5 * 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchAgentSkills(projectId as number, versionId as number, sessionApiClient(), signal),
  });
}

export function useToolkitTools(projectId: number | null, toolkitId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'toolkit-tools', toolkitId ?? 0],
    enabled: enabled && projectId !== null && toolkitId !== null,
    staleTime: 5 * 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchToolkitTools(projectId as number, toolkitId as number, sessionApiClient(), signal),
  });
}
