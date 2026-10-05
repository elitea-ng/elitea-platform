/**
 * What a run may conclude when the DEPLOYMENT, not the product, lacks a
 * prerequisite.
 *
 * ## The two targets
 *
 * The e2e rig (`scripts/e2e-stack.sh`) composes every feature these journeys
 * cover. A deployed instance driven by `playwright.live.config.ts` does not
 * have to: it may leave a feature gate off (`ELITEA_INDEX_TYPES_ENABLED`,
 * `ELITEA_APPLICATION_SKILLS_ENABLED`, `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED`),
 * disallow project-own LLMs, serve its own brand pack, or refuse a stand-in
 * host at its egress allowlist. Those are deployment decisions, and a journey
 * that fails on them reports an environment, not a defect.
 *
 * ## Why every skip here is gated on the live target
 *
 * "The route answered 404, so skip" is the shape that lets a real regression
 * read as a pass: on the rig the same 404 IS the defect. So a helper below only
 * ever SKIPS when the run declared itself live (`E2E_TARGET=live`, which
 * `playwright.live.config.ts` sets) and did not ask to re-admit env-dependent
 * cases for triage (`LIVE_INCLUDE_ENV_DEPENDENT=1`). On the rig no probe is
 * even sent and nothing is skipped — the journey runs and fails on its own
 * assertion, exactly as before.
 */
import { test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

// The rule itself is pure and unit-tested in `scripts/e2e-live-assumptions.test.mjs`.
import {
  catalogueListsModel,
  discoveryAbsentAnswer,
  refusedOnLiveTarget,
  shouldSkipForDeployment,
} from '../../scripts/lib/e2e-live-assumptions.mjs';
import { API_BASE, DEFAULT_PROJECT_ID } from './api';

/**
 * Skip the running test (or every test of the describe whose `beforeEach`
 * calls it) on ANY live run — triage included — because it writes state real
 * users of a shared instance see: it publishes into the public catalogue,
 * flips a platform flag, invites a user. Unlike `skipOnLiveTarget`,
 * `LIVE_INCLUDE_ENV_DEPENDENT=1` does not re-admit it: that switch exists to
 * triage cases that need the rig, never to let one harm the instance.
 *
 * The selection scan in `scripts/e2e-live-assumptions.test.mjs` fails when a
 * live-selected spec writes such state outside a case guarded by this.
 */
export function neverOnLiveTarget(reason: string): void {
  test.skip(refusedOnLiveTarget(process.env), `live safety: ${reason}`);
}

// Pure, so it is unit-tested beside the rest (`scripts/e2e-live-assumptions.test.mjs`).
export { rethrowSkipAfterCleanup } from '../../scripts/lib/e2e-live-assumptions.mjs';

/**
 * Skip the running test when `absent` is true on a live target. On the rig it
 * does nothing, so the test goes on to fail on its own assertion.
 *
 * Call it from inside a test or a `beforeEach`.
 */
export function skipWhenDeploymentLacks(absent: boolean, reason: string): void {
  test.skip(shouldSkipForDeployment(absent, process.env), `deployment: ${reason}`);
}

/**
 * Skip unconditionally on a live target: the case needs something only the rig
 * provides (a mock host the live egress refuses, a compiled-in brand, a stack
 * with no runtime plane). Same triage escape as above.
 */
export function skipOnLiveTarget(reason: string): void {
  skipWhenDeploymentLacks(true, reason);
}

/**
 * `skipWhenDeploymentLacks` for a prerequisite that takes a request to find
 * out. The probe is not even sent unless a skip could follow, so a rig run
 * pays nothing for it — no request, no wait.
 */
async function skipWhenProbeFinds(probe: () => Promise<boolean>, reason: string): Promise<void> {
  if (!shouldSkipForDeployment(true, process.env)) return;
  skipWhenDeploymentLacks(await probe(), reason);
}

/** The status a GET answers, for a probe that only needs to know if a route is served. */
async function probeStatus(request: APIRequestContext, url: string): Promise<number> {
  const response = await request.get(url);
  return response.status();
}

/**
 * `GET /elitea_core/index_types/prompt_lib/{projectId}` answers 404 when
 * `ELITEA_INDEX_TYPES_ENABLED` is off — the gate every index and
 * attachment-type journey stands on.
 */
export async function skipWhenIndexTypesAbsent(
  request: APIRequestContext,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await skipWhenProbeFinds(
    async () =>
      (await probeStatus(request, `${API_BASE}/elitea_core/index_types/prompt_lib/${projectId}`)) === 404,
    'GET index_types answers 404 (ELITEA_INDEX_TYPES_ENABLED is off), so nothing can be indexed',
  );
}

/**
 * `GET /elitea_core/application_skills/prompt_lib/{projectId}/{versionId}`
 * answers 404 when `ELITEA_APPLICATION_SKILLS_ENABLED` is off. Asked about a
 * version the caller just created, so a 404 cannot mean "no such version".
 */
export async function skipWhenApplicationSkillsAbsent(
  request: APIRequestContext,
  versionId: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await skipWhenProbeFinds(
    async () =>
      (await probeStatus(
        request,
        `${API_BASE}/elitea_core/application_skills/prompt_lib/${projectId}/${versionId}`,
      )) === 404,
    'GET application_skills answers 404 (ELITEA_APPLICATION_SKILLS_ENABLED is off)',
  );
}

/**
 * `GET /elitea_core/toolkit_available_tools/prompt_lib/{projectId}/{toolkitId}`
 * answers 503 `{error: "toolkit discovery unavailable"}` when runtime toolkit
 * discovery is not composed (`ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED=false`).
 * Any other answer lets the journey run and fail on its own assertion.
 */
export async function skipWhenToolkitDiscoveryAbsent(
  request: APIRequestContext,
  toolkitId: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await skipWhenProbeFinds(async () => {
    const response = await request.get(
      `${API_BASE}/elitea_core/toolkit_available_tools/prompt_lib/${projectId}/${toolkitId}`,
    );
    return discoveryAbsentAnswer(response.status(), await response.text());
  }, 'GET toolkit_available_tools answers 503 "toolkit discovery unavailable" (ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED is off)');
}

/**
 * A project's OWN embedding model is invisible to its own catalogue.
 *
 * With `ELITEA_ALLOW_PROJECT_OWN_LLMS=false` the row is stored but left at
 * `status_ok = false` for every project except the public one, and every
 * catalogue reader selects `status_ok = true` — so a toolkit naming the model
 * is refused with `configuration_model_not_found`. Read through the route the
 * picker uses, a few times, because admission settles just after the write.
 */
export async function skipWhenProjectOwnModelsDisallowed(
  request: APIRequestContext,
  modelName: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await skipWhenProbeFinds(async () => {
    const url = `${API_BASE}/configurations/models/${projectId}?section=embedding`;
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const response = await request.get(url);
      const body = await response.text();
      // Only a SERVED catalogue that lacks the row says "disallowed"
      // (`catalogueListsModel` throws on a 403/500 or a non-JSON body): a
      // read fault must not read as a deployment choice.
      if (catalogueListsModel(response.status(), body, modelName)) return false;
      await new Promise((resolve) => setTimeout(resolve, 1_000));
    }
    return true;
  }, 'the project cannot see its own embedding model (ELITEA_ALLOW_PROJECT_OWN_LLMS=false)');
}
