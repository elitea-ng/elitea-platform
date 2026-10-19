/**
 * API-RC: which worker this deployment runs, and what it cannot do (#865,
 * #866).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS COVERS
 * ─────────────────────────────────────────────────────────────────────────────
 * Before `GET /elitea_core/runtime_capabilities` existed, nothing in the
 * product could tell a user or an administrator which worker image a
 * deployment ran, or what that worker could not do — 17 (really 18, see
 * `internalTools.test.ts`'s own note) toolkit types the configured worker
 * cannot build disappeared from the type picker with no explanation
 * (`metadata.hidden`, filtered client-side), and six internal chat tools the
 * Rust worker cannot run answered as if nothing had happened. This journey
 * asserts the endpoint answers, with the shape the web app's
 * `shared/api/runtimeCapabilities.ts` reads: `worker`, `internal_tools` (the
 * six toggleable names from `features/agents/lib/internalTools.ts`), and
 * `hidden_toolkit_types`.
 *
 * It does NOT assert a specific worker — the e2e stack's configured worker is
 * a deployment fact this suite does not control — only that the answer is
 * INTERNALLY CONSISTENT: on a Rust deployment each tool the worker skips
 * answers `false` and at least one toolkit type is withheld; on a Python
 * deployment every one of the six answers `true`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. One read, no writes.
 */
import { test, expect } from '@playwright/test';

import { API_BASE, describeRefusal } from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

// Deliberately NOT project-scoped — same persona choice as api.health.spec.ts:
// the endpoint answers a deployment-wide fact, so any signed-in caller proves
// the claim.
test.use({ storageState: STORAGE_STATE.admin });

// The toggleable internal chat tools this endpoint reports on —
// `features/agents/lib/internalTools.ts`'s `INTERNAL_TOOLS_LIST`, minus
// `pyodide`, which is not reported (see
// `internal/api/v2/toolkits/capabilities_handler.go`'s own doc comment). Three
// are skipped by the Rust worker; the rest it serves natively.
const RUST_SKIPPED_INTERNAL_TOOLS = ['image_generation', 'internal_mcp', 'swarm'] as const;
// data_analysis is served by Rust as structured table tools (ADR-0033).
const RUST_SERVED_INTERNAL_TOOLS = ['planner', 'lazy_tools_mode', 'attachments', 'data_analysis'] as const;
const REPORTED_INTERNAL_TOOLS = [...RUST_SKIPPED_INTERNAL_TOOLS, ...RUST_SERVED_INTERNAL_TOOLS] as const;

interface RuntimeCapabilities {
  readonly worker: string;
  readonly internal_tools: Readonly<Record<string, boolean>>;
  readonly hidden_toolkit_types: readonly string[];
}

test('API-RC1: the endpoint answers the shape the web app reads, for a signed-in caller', async ({
  request,
}) => {
  const resp = await request.get(`${API_BASE}/elitea_core/runtime_capabilities`);
  expect(resp.status(), `${await resp.text()}${await describeRefusal(resp)}`).toBe(200);
  expect(resp.headers()['content-type'] ?? '').toContain('application/json');

  const body = (await resp.json()) as RuntimeCapabilities;

  expect(['rust', ''], `worker=${JSON.stringify(body.worker)}`).toContain(body.worker);
  expect(Array.isArray(body.hidden_toolkit_types), JSON.stringify(body)).toBe(true);

  // Every reported name is present with a boolean answer — not
  // merely absent-and-truthy, which the pre-#866 UI would have read as
  // "available" (`isInternalToolAvailable`'s own "unset means available"
  // rule) and hidden the gap all over again.
  for (const name of REPORTED_INTERNAL_TOOLS) {
    expect(typeof body.internal_tools[name], `internal_tools.${name}=${JSON.stringify(body.internal_tools[name])}`).toBe(
      'boolean',
    );
  }
});

test('API-RC2: the answer is internally consistent with the configured worker', async ({ request }) => {
  const resp = await request.get(`${API_BASE}/elitea_core/runtime_capabilities`);
  expect(resp.status(), `${await resp.text()}${await describeRefusal(resp)}`).toBe(200);
  const body = (await resp.json()) as RuntimeCapabilities;

  if (body.worker === 'rust') {
    // #866's core claim, machine-checked: what the Rust worker still skips
    // is reported false; what it serves natively is reported true.
    for (const name of RUST_SKIPPED_INTERNAL_TOOLS) {
      expect(body.internal_tools[name], `internal_tools.${name} on rust`).toBe(false);
    }
    for (const name of RUST_SERVED_INTERNAL_TOOLS) {
      expect(body.internal_tools[name], `internal_tools.${name} on rust`).toBe(true);
    }
    // #865's core claim: at least one catalogued type is withheld — the
    // Rust worker materializes 22 of the 52 catalogued families.
    expect(body.hidden_toolkit_types.length, JSON.stringify(body.hidden_toolkit_types)).toBeGreaterThan(0);
  }
});
