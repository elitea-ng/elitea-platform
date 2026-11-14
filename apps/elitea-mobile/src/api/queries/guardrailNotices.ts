import { sessionApiClient, type ApiClient } from '@/api/client';
import type { components } from '@/api/generated/elitea.gen';

/**
 * What the content guardrails did to one run (ADR-0043 decision 1, issue 2012),
 * from `listRunGuardrailNotices`:
 *
 *   GET /elitea_core/guardrail_notices/prompt_lib/{project_id}/{run_id}
 *
 * A notice is typed (kind, stage, action, guardrail name, labels such as
 * `email`) and never holds the inspected text or the matched value. The
 * gateway writes its verdicts up to a second after the request, so a read
 * waits `NOTICE_SETTLE_MS` first. A failed read is an empty list: a notice is
 * a courtesy and must never break the answer it sits under.
 */
export type GuardrailNotice = components['schemas']['RunGuardrailNotice'];

export const NOTICE_SETTLE_MS = 1500;

const RUN_ID = /^[A-Za-z0-9._-]{1,128}$/u;

function pause(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener('abort', () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

export async function fetchGuardrailNotices(
  projectId: number,
  runId: string,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
  settleMs: number = NOTICE_SETTLE_MS,
): Promise<GuardrailNotice[]> {
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !RUN_ID.test(runId)) return [];
  if (settleMs > 0) await pause(settleMs, signal);
  if (signal?.aborted) return [];
  try {
    const { data, response } = await client.GET('/elitea_core/guardrail_notices/prompt_lib/{project_id}/{run_id}', {
      params: { path: { project_id: String(projectId), run_id: runId } },
      signal,
    });
    if (!response.ok) return [];
    return Array.isArray(data?.notices) ? data.notices.filter((n) => typeof n?.kind === 'string' && typeof n.guardrail === 'string') : [];
  } catch {
    return [];
  }
}
