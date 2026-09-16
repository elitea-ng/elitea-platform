/**
 * Project Context: the raw read/write pair, and the mutex that fixing #888
 * made necessary.
 *
 * ## Why a lock exists here now
 *
 * Project Context is ONE ROW PER PROJECT (`p_<id>.configuration` where
 * `type = 'project_context'`), and every journey persona works inside project
 * 1. While the write silently no-opped (#888) that did not matter: the row
 * never changed, so "the project starts empty and stays empty" was true for
 * every test, in any order, at any concurrency — which is exactly what the
 * old `settings.project-context.spec.ts` header wrote down and relied on.
 *
 * Making the write real (this package) turns that shared row into shared
 * MUTABLE state, under `fullyParallel: true`, across two spec files:
 *
 *  - `settings.project-context.spec.ts` — saves through the UI, and asserts
 *    the toggle's initial position and the empty state's read-only branch,
 *    both of which are properties OF THE ROW;
 *  - `settings.p13-project-context.spec.ts` — PUTs a payload through the API
 *    and reads it straight back.
 *
 * Two of those interleaved is a clobber, not a flake that a retry fixes: the
 * value one test polls for is overwritten by another test's save. So both
 * files take this mutex for the whole of each test and start from a KNOWN
 * row (`resetProjectContext`), rather than from whatever the previous test
 * left behind.
 *
 * ## The mutex
 *
 * `mutex.ts`'s named lock, under the name `project-context`. It is a second,
 * independent window rather than a widening of `platformFlags.ts`'s: a
 * project-context test has no reason to queue behind an MCP-flag test.
 */
import { request as apiRequest, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { API_BASE } from './api';
import { acquireNamedLock } from './mutex';

/** What `{content, enabled}` the route answers for a project that has never saved one. */
export interface ProjectContextState {
  readonly content: string;
  readonly enabled: boolean;
}

/**
 * The state every project on a freshly seeded stack is in, and the state
 * `resetProjectContext` puts one back into.
 *
 * `enabled: false` with empty content is what `ProjectContext.tsx`'s
 * `deriveShowFlags` treats as "nothing saved": the empty state renders and
 * the editor does not mount until the toggle is flipped or Create is clicked.
 */
export const EMPTY_PROJECT_CONTEXT: ProjectContextState = { content: '', enabled: false };

export function projectContextUrl(projectId: string): string {
  return `${API_BASE}/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
}

/**
 * Reads the row the SERVER holds, straight off the route.
 *
 * The body is the plain `{content, enabled}` object — NOT `{data: {...}}`.
 * The `{data}` wrapper belongs to the browser client (`eliteaFetch` builds
 * it; `projectContextApi.ts` unwraps it), so a `page.request` reader that
 * reaches for `body.data.content` gets `undefined` and, with a `?? ''`
 * fallback, reports "" for every project forever — indistinguishable from
 * the very no-op-save defect (#888) such a reader is usually written to
 * measure. Verified against the running stack with curl before this helper
 * was written.
 */
export async function readProjectContext(
  api: APIRequestContext,
  projectId: string,
): Promise<ProjectContextState> {
  const response = await api.get(projectContextUrl(projectId));
  if (!response.ok()) {
    throw new Error(
      `GET project context for project ${projectId}: ${String(response.status())} ${await response.text()}`,
    );
  }
  const body = (await response.json()) as Partial<ProjectContextState>;
  return { content: body.content ?? '', enabled: body.enabled ?? false };
}

/** PUTs `{content, enabled}` and throws on any non-200 — a refused save must not read as an empty one. */
export async function writeProjectContext(
  api: APIRequestContext,
  projectId: string,
  state: ProjectContextState,
): Promise<void> {
  const response = await api.put(projectContextUrl(projectId), { data: state });
  if (!response.ok()) {
    throw new Error(
      `PUT project context for project ${projectId}: ${String(response.status())} ${await response.text()}`,
    );
  }
}

/**
 * Puts one project's row back to the seeded default, as ADMIN.
 *
 * Admin rather than the calling test's own persona because the viewer
 * persona (`STORAGE_STATE.viewer`, which holds `models.project_context.view`
 * and specifically NOT `.edit`) must still be able to arrange its own
 * precondition — the whole point of that persona is that it cannot write.
 */
export async function resetProjectContext(projectId: string): Promise<void> {
  await seedProjectContextAsAdmin(projectId, EMPTY_PROJECT_CONTEXT);
}

/** Saves one project's row as admin, for a test whose own persona may not write. */
export async function seedProjectContextAsAdmin(
  projectId: string,
  state: ProjectContextState,
): Promise<void> {
  const admin = await apiRequest.newContext({
    baseURL: BASE_URL,
    storageState: STORAGE_STATE.admin,
  });
  try {
    await writeProjectContext(admin, projectId, state);
  } finally {
    await admin.dispose();
  }
}

/* ── the mutex ─────────────────────────────────────────────────────────── */

/**
 * Takes the project-context window and returns its release.
 *
 * Call it in `beforeEach` and release in `afterEach`, so a test that throws
 * mid-way still frees the row for the next one.
 *
 * The mechanism lives in `mutex.ts` and is shared with the other journeys that
 * act on state there is exactly one of; the NAME is what keeps them from
 * queueing behind each other.
 */
export async function acquireProjectContextLock(): Promise<() => Promise<void>> {
  return acquireNamedLock('project-context');
}
