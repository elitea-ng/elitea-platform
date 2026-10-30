#!/usr/bin/env node
/**
 * Reports how much of `parity/screenshot-index.json` the `@visual` suite
 * actually covers, and fails when a route the index marks `wiringStatus: wired`
 * has no spec.
 *
 * Why this exists: the `visual` job passing says "no snapshot changed". It says
 * nothing about how many screens have a snapshot at all. Without this, 5 covered
 * routes and 29 covered routes produce the identical green tick — and the whole
 * point of the surrounding effort is that a green gate must not imply coverage
 * it does not have.
 *
 * Only `wired` is enforced. A route still marked `ready`/`needs-route-state`/
 * `blocked-codegen`/`hybrid-defect` renders scaffolding, and a baseline of
 * scaffolding is worse than no baseline: it makes the stub the official
 * reference and goes green forever after.
 *
 * A REFUSAL IS NOT SCAFFOLDING (issue #229). Several admin screens render
 * "unavailable, and here is why" and nothing else — Service Descriptors
 * entirely, and every one of Configuration's twelve sections. Those ARE
 * baselined, deliberately, and the distinction is not a loophole: scaffolding is
 * unfinished UI whose eventual screen is meant to look different, so pinning it
 * freezes the wrong thing; a server-declared refusal is the FINISHED screen, and
 * pinning it is what makes the regression visible if someone later wires the
 * endpoint to a stub that answers 200. Both pages render the SERVER's sentence
 * rather than a copy they carry, which is why the baseline moves when the server
 * changes its answer. Contrast `/help-center` in the EXEMPT map below: there the
 * content is MISSING (nothing has been configured), not refused, so a baseline
 * would pin an unconfigured tenant rather than a decision.
 *
 * Run: node scripts/check-visual-coverage.mjs [--json]
 */
import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkFloors } from './lib/gate-floor.mjs';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const INDEX = path.join(appRoot, 'parity/screenshot-index.json');
const VISUAL_DIR = path.join(appRoot, 'e2e/visual');

/*
 * Floors (issue #528). Every number below is a plausibility bar, not a target.
 *
 * The gate reads three subject sets, and each one can go empty without any
 * other check noticing:
 *   - `index.shots` — rename the key, or ship `"shots": []`, and `wired` is
 *     empty, `uncoveredRoutes` is empty, and the script prints "every wired
 *     route has a spec" over nothing at all.
 *   - the `wired` subset — change the `wiringStatus` value and the same thing
 *     happens with the index still full.
 *   - `e2e/snapshots` — `dirStats` reports 0 bytes for an absent directory, so
 *     deleting every baseline met the size budget.
 *
 * Measured on 2026-08-28: 76 indexed shots, 53 of them `wired`, 38 committed
 * baseline PNGs. Ordinary work moves those up. Lower a floor only together
 * with the measured number that replaces it.
 */
const MIN_INDEXED_SHOTS = 60;
const MIN_WIRED_SHOTS = 40;
const MIN_BASELINE_FILES = 24;

const index = JSON.parse(readFileSync(INDEX, 'utf8'));
const shots = index.shots ?? [];

if (!existsSync(VISUAL_DIR)) {
  console.error('check-visual-coverage: e2e/visual/ does not exist — no screen is compared to any reference.');
  process.exit(1);
}

const specSource = readdirSync(VISUAL_DIR)
  .filter((f) => f.endsWith('.spec.ts'))
  .map((f) => readFileSync(path.join(VISUAL_DIR, f), 'utf8'))
  .join('\n');

if (specSource.trim() === '') {
  console.error('check-visual-coverage: e2e/visual/ contains no *.spec.ts.');
  process.exit(1);
}

/**
 * Coverage is DECLARED, not inferred: a spec claims a route with an
 * `@covers <route>` annotation, and only exactly-matching index routes count.
 *
 * Inferring it from navigation was the first design and it lied. Matching a
 * route's static prefix meant `/chat/:conversationId` counted as covered because
 * some spec navigated to `/app/chat` — reporting 15/15 wired shots while the
 * spec file itself documented the four `:conversationId` shots as NOT covered.
 * A coverage gate that over-reports is worse than none, since it launders the
 * gap it was built to expose.
 */
const declared = new Set(
  [...specSource.matchAll(/@covers\s+(\S+)/g)].map((m) => m[1]),
);

function isCovered(route) {
  return typeof route === 'string' && declared.has(route);
}

/**
 * Routes that are `wired` in the FRONTEND but are not snapshotted, because
 * reaching them with real data depends on something outside this suite. Each
 * needs an issue, and each is printed on every run — an exemption nobody sees is
 * just a silent gap.
 *
 * Keep each reason literally true. The first entry here asserted a backend "has
 * no implementation" on the strength of a grep that could not have found one,
 * and that sentence was then copied into a spec file and an issue before anyone
 * checked it.
 *
 * This is not a general escape hatch. Adding an entry means claiming a specific,
 * externally-tracked blocker; "the spec is fiddly" is not one.
 */
const EXEMPT = new Map([
  // The original reason here was wrong twice over, and both errors are worth
  // keeping visible. It claimed ConvsRepo "has no implementation" — the check
  // behind that grepped for the interface NAME, and Go interfaces are satisfied
  // structurally, so a 951-line implementation was invisible to it. It then
  // claimed ConvsRepo "is never wired", which stopped being true in 3b73273.
  // What remains true is only that this route has never been snapshotted, and
  // nobody has confirmed a conversation can actually be created end-to-end.
  ['/chat/:conversationId', 'not snapshotted — needs a deterministic conversation list, NOT a missing backend. POST /api/v2/elitea_core/conversations/prompt_lib/1 was verified returning 201 against the running stack; the route renders (chat.tsx supplies ChatWithEditors + <Outlet/>, so the child\'s `component: () => null` is by design). The obstacle is that seeding a conversation per run grows the sidebar list, so the snapshot would differ every time. It is now closer to tractable: the sidebar is mounted (#128) and /chat\'s own landmark ("Still no conversations created.") is the empty branch of that very list, so a seed producing a FIXED conversation set unblocks all four shots.'],
  // Wired at the route layer as of the #61 re-classification, and still exempt
  // — but for a NARROWER reason since the admin Configuration port (#200).
  //
  // `useResourcesConfig` used to make no request at all, because the endpoint
  // behind it returned chat and upload limits rather than resource links. It
  // now reads `GET /admin/plugin_config_values/prompt_lib/resources`, which
  // serves the section an administrator edits on Admin > Configuration >
  // Resources, and journey 36g asserts that round trip end to end.
  //
  // What is left is a SEEDING question rather than a wiring one, which is the
  // same shape as `/chat/:conversationId` above: this stack configures no
  // resource links, so the cards still render "No links configured" and a
  // baseline would still make an unconfigured screen the official reference.
  // Seed a fixed set of links and the route becomes coverable.
  // Issue #229. Every other admin route got a baseline in that change; this one
  // is the single exception and the reason is determinism, not wiring — the page
  // reads the database and renders it correctly.
  //
  // The obstacle is that EVERY visible value on this screen is a function of the
  // wall clock at seed time, so a baseline taken against one stack cannot match
  // a stack seeded at a different time of day — which is every other stack,
  // including every CI run. Both DateRangeFields render today's date; the Time
  // column is second-resolution local time; and the heatmap's column geometry
  // and per-cell alpha are computed from where the four seeded rows fall inside
  // the range (`cellAlpha` re-scales every cell against the grid's max, so
  // nothing about that chart is stable).
  //
  // WHICH day it is no longer belongs on that list. #214 pinned one timezone for
  // the browser and the seed, and journey 29 freezes its clock to the day the
  // fixture was written on, so the `Today` window and the rows agree by
  // construction. That fixes correctness, not stability: the rows still land at
  // whatever minute the seed ran, which is what a baseline cannot survive.
  //
  // Masking is not a way out: what would have to be masked is the entire content
  // area, which is the whole page. What WOULD unblock it is a fixture at a fixed
  // absolute timestamp — journey 29's frozen clock is half of that already —
  // read through a deterministic range.
  ['/admin/app/audit', 'not snapshotted — the route is wired and renders real seeded data (journey 29 covers it), but its default screen is entirely wall-clock-derived: second-resolution timestamps, two date fields showing today, and a heatmap whose bucket geometry and per-cell alpha depend on where the seeded rows fall in the range. WHICH day is no longer part of it — #214 pinned one timezone for browser and seed and froze journey 29 to the seeded day — but the rows still land at whatever minute the seed ran, so a baseline could only ever match the stack that produced it. Unblocking it needs a fixed-timestamp fixture, not a mask.'],
  // Issue 1495 (ADR-0042 decision 4). New in the change that added it; the
  // baseline is regenerated through the ci-web-e2e workflow_dispatch, never
  // locally, so the page enters exempt and leaves when that run lands.
  ['/admin/app/retention', 'not snapshotted yet — new page (issue 1495). The policy table renders the seeded defaults from shared migration 0195, so the screen is deterministic; add a spec to admin.visual.spec.ts and regenerate the baseline through ci-web-e2e workflow_dispatch to remove this entry.'],
  // Issue 1497 (ADR-0040 decision 1). New in the change that added it, like
  // retention above: the baseline comes from a ci-web-e2e workflow_dispatch.
  ['/admin/app/agent-registry', 'not snapshotted yet — new page (issue 1497). The table lists the seeded agents with their backfilled tier, so pin it with a landmark on agent-registry-table, add a spec to admin.visual.spec.ts and regenerate the baseline through ci-web-e2e workflow_dispatch to remove this entry.'],
  // Issue 1501 (ADR-0044 decision 1). New in the change that added it, like
  // the two above: the baseline comes from a ci-web-e2e workflow_dispatch.
  ['/admin/app/connector-requests', 'not snapshotted yet — new page (issue 1501). The E2E stack seeds no connector request, so the reference is the empty submitted queue; pin it with a landmark on connector-requests-table, add a spec to admin.visual.spec.ts and regenerate the baseline through ci-web-e2e workflow_dispatch to remove this entry.'],
  // Issue 1501 (ADR-0044 decision 5), merged beside the connector queue.
  ['/admin/app/credential-inventory', 'not snapshotted yet — new page (issue 1501). Its last-use and staleness columns depend on when the stack seeded its tokens, so pin it with a landmark and a fixed-timestamp fixture, add a spec to admin.visual.spec.ts and regenerate the baseline through ci-web-e2e workflow_dispatch to remove this entry.'],
  ['/admin/app/permission-trimming', 'not snapshotted yet — new page (issue 1501). The E2E stack seeds no trimming candidate, so the reference is the empty report; add a spec to admin.visual.spec.ts and regenerate the baseline through ci-web-e2e workflow_dispatch to remove this entry.'],
  ['/help-center', 'not snapshotted — needs seeded resource links, NOT a missing backend. `useResourcesConfig` reads GET /admin/plugin_config_values/prompt_lib/resources, which the admin port (#200) made serve real values; the section is authored on Admin › Features and journey 36g asserts the round trip. This stack configures none, so a baseline would still pin an unconfigured screen; seed a fixed set of links and it is coverable. Measured meanwhile, its stalled and loaded renders are byte-identical, so it will need only a mount guard, not a data landmark.'],
]);

const wired = shots.filter((s) => s.wiringStatus === 'wired');
const coveredWired = wired.filter((s) => isCovered(s.route));
const uncoveredWired = wired.filter((s) => !isCovered(s.route) && !EXEMPT.has(s.route));
const exemptWired = wired.filter((s) => EXEMPT.has(s.route));
const coveredAll = shots.filter((s) => isCovered(s.route));

const uncoveredRoutes = [...new Set(uncoveredWired.map((s) => s.route))].sort();

/*
 * Baseline weight tripwire — the counting half.
 *
 * PNGs do not delta-compress, so every revision of a baseline stores a full
 * copy in history. The byte budget below guards the top end. `files` guards
 * the bottom end: an absent or emptied `e2e/snapshots` reads as 0 bytes, which
 * met the budget and passed.
 */
const SNAPSHOT_BUDGET_BYTES = 12 * 1024 * 1024;
const SNAPSHOT_DIR = path.join(appRoot, 'e2e/snapshots');

function dirStats(dir) {
  if (!existsSync(dir)) return { files: 0, bytes: 0 };
  let files = 0;
  let bytes = 0;
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      const inner = dirStats(p);
      files += inner.files;
      bytes += inner.bytes;
    } else {
      files += 1;
      bytes += statSync(p).size;
    }
  }
  return { files, bytes };
}

const snapshot = dirStats(SNAPSHOT_DIR);

if (process.argv.includes('--json')) {
  console.log(JSON.stringify({
    totalShots: shots.length,
    coveredShots: coveredAll.length,
    wiredShots: wired.length,
    coveredWiredShots: coveredWired.length,
    baselineFiles: snapshot.files,
    baselineBytes: snapshot.bytes,
    uncoveredWiredRoutes: uncoveredRoutes,
    ok: uncoveredRoutes.length === 0,
  }, null, 2));
}

// Always state the real numbers, pass or fail. A reader must never have to infer
// coverage from the exit code.
console.log(
  `check-visual-coverage: ${coveredAll.length}/${shots.length} indexed shots have a @visual spec ` +
  `(${coveredWired.length}/${wired.length} of the 'wired' ones, ${exemptWired.length} exempt).`,
);

// The floors come BEFORE the coverage verdict. An empty index makes that
// verdict vacuously green, so the count has to be stated and refused first.
const floors = checkFloors('check-visual-coverage', [
  { subject: 'indexed shots in parity/screenshot-index.json', observed: shots.length, floor: MIN_INDEXED_SHOTS },
  { subject: "indexed shots with wiringStatus 'wired'", observed: wired.length, floor: MIN_WIRED_SHOTS },
  { subject: 'committed baseline files under e2e/snapshots', observed: snapshot.files, floor: MIN_BASELINE_FILES },
]);
for (const line of floors.lines) console.log(line);
if (!floors.ok) {
  console.error(floors.error);
  process.exit(2);
}

// Exemptions are printed BEFORE the verdict, pass or fail, so they can never
// read as covered.
if (exemptWired.length > 0) {
  const routes = [...new Set(exemptWired.map((s) => s.route))].sort();
  console.log(`check-visual-coverage: ${exemptWired.length} shot(s) across ${routes.length} route(s) EXEMPT — wired in the frontend, NOT snapshotted. Reason per route:`);
  for (const r of routes) console.log(`  ${r} — ${EXEMPT.get(r)}`);
}

if (uncoveredRoutes.length > 0) {
  console.error('check-visual-coverage: FAIL — these routes are wired but have no @visual spec:');
  for (const r of uncoveredRoutes) console.error(`  ${r}`);
  console.error('Add a spec in e2e/visual/, or correct wiringStatus if the route is not actually wired.');
  process.exit(1);
}

/*
 * Baseline weight tripwire — the budget half. The count half runs above, with
 * the other floors.
 *
 * PNGs do not delta-compress, so every revision of a baseline stores a full copy
 * in history. Today: 5 baselines, ~400KB, and full coverage of all 63 indexed
 * shots projects to ~5MB — comfortably inside what git handles well, so this
 * repo deliberately does NOT use git-lfs for them.
 *
 * That decision is fine until it silently isn't. LFS would also introduce a
 * failure mode worse than the size it solves: without LFS configured on a
 * runner, checkout yields POINTER FILES, and either the comparison fails
 * confusingly or someone "fixes" it with --update-snapshots and overwrites every
 * pointer with a fresh baseline — a green suite whose references mean nothing.
 * Since the visual job runs on release tags only, that could sit undetected.
 *
 * So: fail loudly at a threshold that is generous for full coverage but far
 * below the point where lfs is genuinely warranted (~100MB of binary history).
 * If this fires, decide deliberately — prune, or adopt lfs with the pointer
 * hazard handled — rather than discovering the weight later.
 */
const snapshotBytes = snapshot.bytes;
const mb = (n) => `${(n / 1024 / 1024).toFixed(2)}MB`;
console.log(
  `check-visual-coverage: ${snapshot.files} baselines occupy ${mb(snapshotBytes)} of a ${mb(SNAPSHOT_BUDGET_BYTES)} budget.`,
);
if (snapshotBytes > SNAPSHOT_BUDGET_BYTES) {
  console.error(
    `check-visual-coverage: FAIL — e2e/snapshots is ${mb(snapshotBytes)}, over the ${mb(SNAPSHOT_BUDGET_BYTES)} budget.\n` +
    'Decide deliberately: prune baselines that no longer earn their weight, or adopt git-lfs —\n' +
    'and if lfs, make CI fail when a baseline is a pointer file, because --update-snapshots\n' +
    'against pointers silently destroys the whole reference set.',
  );
  process.exit(1);
}

console.log('check-visual-coverage: OK — every wired route has a spec.');
console.log(
  `NOTE: ${shots.length - coveredAll.length} indexed shots remain uncovered (routes not yet 'wired', ` +
  'plus light-theme and collapsed-rail variants). A green `visual` job does NOT mean the UI is verified.',
);
