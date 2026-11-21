#!/usr/bin/env node
// The compose E2E legs ci-web-e2e.yml runs, and which of them a run needs.
//
// LEGS is the source of truth for two matrices: ci-web-e2e.yml's `e2e` job
// and its `chat-stream-rust` job read `fromJSON` of the include list this
// script prints (ci-build-once.yml's lane filter job writes it to its
// outputs). A leg the run does not need is left out of the list, so it
// starts no runner. It used to stay in a literal matrix and skip every step
// (RUN_LEG): on a pull request the pool runs, that was eight hosted runners
// a run that did nothing.
//
// ci-web-e2e-native.yml's `e2e` matrix is the same seven legs, written out
// (apps/elitea-web/scripts/e2e-journey-shape.test.mjs holds the two equal).
//
//   EVENT_NAME=pull_request POOLED=true REACHES_WEBKIT=false node scripts/ci/e2e-legs.mjs
//   e2e-legs=[...]
//   chat-stream-rust-legs=[...]
import { pathToFileURL } from 'node:url';

// Shards and weights: the reasoning lives on each job in ci-web-e2e.yml.
// `weights` is PWTEST_SHARD_WEIGHTS for the leg (one weight per shard).
export const LEGS = Object.freeze({
  e2e: [
    { engine: 'chromium', leg: 'chromium-1of2', shard: '1/2', weights: '523:242' },
    { engine: 'chromium', leg: 'chromium-2of2', shard: '2/2', weights: '523:242' },
    { engine: 'webkit', leg: 'webkit-1of3', shard: '1/3', weights: '443:164:158' },
    { engine: 'webkit', leg: 'webkit-2of3', shard: '2/3', weights: '443:164:158' },
    { engine: 'webkit', leg: 'webkit-3of3', shard: '3/3', weights: '443:164:158' },
    { engine: 'platform-flags', leg: 'platform-flags', shard: '', weights: '' },
    { engine: 'platform-flags-webkit', leg: 'platform-flags-webkit', shard: '', weights: '' },
  ],
  'chat-stream-rust': [
    { leg: '1of3', shard: '1/3', weights: '61:16:33' },
    { leg: '2of3', shard: '2/3', weights: '61:16:33' },
    { leg: '3of3', shard: '3/3', weights: '61:16:33' },
  ],
});

/** The leg that stays as the compose image smoke where the pool runs the rest natively. */
export const SMOKE_LEG = Object.freeze({ e2e: 'chromium-1of2', 'chat-stream-rust': '1of3' });

const WEBKIT_ENGINES = new Set(['webkit', 'platform-flags-webkit']);

/**
 * The legs of `lane` a run needs.
 * - A pull request the pool runs (`pooled`) needs only the smoke leg: the
 *   native workflow runs every leg as processes.
 * - Every other run (a fork's or Dependabot's pull request, ENG_RUNNER off,
 *   the nightly run) needs every leg.
 * - The e2e webkit legs also need `reachesWebkit` (a web, harness or client
 *   change; always true off a pull request).
 */
export function selectLegs(lane, { event, pooled, reachesWebkit }) {
  const legs = LEGS[lane];
  if (!legs) throw new Error(`unknown lane ${lane}`);
  const smokeOnly = event === 'pull_request' && pooled;
  return legs.filter((leg) => {
    if (lane === 'e2e' && WEBKIT_ENGINES.has(leg.engine) && !reachesWebkit) return false;
    return !smokeOnly || leg.leg === SMOKE_LEG[lane];
  });
}

const flag = (value) => String(value ?? '').trim() === 'true';

/** The GITHUB_OUTPUT lines for `env`. */
export function outputs(env) {
  const options = { event: env.EVENT_NAME, pooled: flag(env.POOLED), reachesWebkit: flag(env.REACHES_WEBKIT) };
  return Object.keys(LEGS).map((lane) => `${lane}-legs=${JSON.stringify(selectLegs(lane, options))}`);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  if (!process.env.EVENT_NAME) {
    console.error('e2e-legs.mjs: EVENT_NAME is required');
    process.exit(2);
  }
  for (const line of outputs(process.env)) console.log(line);
}
