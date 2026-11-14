/**
 * api/guardrailNoticesApi.ts — what the content guardrails did to one run
 * (ADR-0043 decision 1, issue 2012), through the generated
 * `listRunGuardrailNotices`.
 *
 *   GET /elitea_core/guardrail_notices/prompt_lib/{projectId}/{runId}
 *
 * The answer is typed (kind, stage, action, guardrail name, labels) and never
 * holds the inspected text or the matched value. The gateway writes its
 * verdicts up to a second after the request, so the read waits `settleMs`
 * first. A failed read is an empty list: a notice is a courtesy and must never
 * break the answer it sits under.
 */
import { listRunGuardrailNotices } from '@/shared/api/generated/chat/chat';
import type { RunGuardrailNotice } from '@/shared/api/generated/model';

const PROJECT_ID = /^[1-9][0-9]{0,18}$/u;
const RUN_ID = /^[A-Za-z0-9._-]{1,128}$/u;

/** How long a turn's verdicts may take to reach the table (the gateway flushes once a second). */
const GUARDRAIL_NOTICE_SETTLE_MS = 1500;

/* manifest: chat.listRunGuardrailNotices */
export async function fetchGuardrailNotices(
  projectId: string,
  runId: string,
  settleMs: number = GUARDRAIL_NOTICE_SETTLE_MS,
  signal?: AbortSignal,
): Promise<readonly RunGuardrailNotice[]> {
  if (!PROJECT_ID.test(projectId) || !RUN_ID.test(runId)) return [];
  if (settleMs > 0) {
    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, settleMs);
      signal?.addEventListener('abort', () => { clearTimeout(timer); resolve(); }, { once: true });
    });
  }
  if (signal?.aborted) return [];
  try {
    const envelope = await listRunGuardrailNotices(projectId, runId, signal ? { signal } : undefined);
    const body = (envelope as unknown as { readonly data?: { readonly notices?: unknown } }).data;
    return Array.isArray(body?.notices) ? (body.notices as readonly RunGuardrailNotice[]) : [];
  } catch {
    return [];
  }
}
