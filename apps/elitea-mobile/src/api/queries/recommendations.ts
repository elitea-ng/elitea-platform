import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import type { Agent } from '@/api/queries/agents';
import { queryKeys } from '@/api/queries/keys';

/**
 * Suggested agents for the empty new-chat screen (client contract 1.3,
 * getRecommendations): the project's agents the workspace recommends, at
 * most 10, as `{applications: [{id, name, description, likes}], total}` with
 * a string id. Read online; a failure just shows no suggestions.
 */

export const MAX_RECOMMENDATIONS = 10;

const Row = z.looseObject({
  id: z.union([z.string(), z.number()]),
  name: z.string().max(500),
  description: z.string().max(5000).nullish(),
});
const Envelope = z.looseObject({ applications: z.array(z.unknown()).max(1000) });

/** The recommendation rows as agents; a row whose id is not a positive integer is dropped. */
export function recommendationsOf(data: unknown): Agent[] {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) return [];
  const out: Agent[] = [];
  for (const raw of envelope.data.applications) {
    const row = Row.safeParse(raw);
    if (!row.success) continue;
    const id = typeof row.data.id === 'number' ? row.data.id : /^\d{1,15}$/.test(row.data.id) ? Number(row.data.id) : NaN;
    if (!Number.isSafeInteger(id) || id <= 0 || row.data.name.trim() === '' || out.some((a) => a.id === id)) continue;
    out.push({ id, name: row.data.name.trim(), description: row.data.description?.trim() ?? '' });
    if (out.length >= MAX_RECOMMENDATIONS) break;
  }
  return out;
}

export async function fetchRecommendations(projectId: number, client: ApiClient = sessionApiClient()): Promise<Agent[]> {
  const { data, response } = await client.GET('/elitea_core/recommendations/prompt_lib/{project_id}', {
    params: { path: { project_id: String(projectId) }, query: { limit: MAX_RECOMMENDATIONS } },
  });
  if (!response.ok) return [];
  return recommendationsOf(data);
}

export function useRecommendations(projectId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.recommendations(projectId ?? 0),
    queryFn: () => fetchRecommendations(projectId!),
    enabled: enabled && projectId !== null,
    staleTime: 5 * 60_000,
    retry: false,
  });
}
