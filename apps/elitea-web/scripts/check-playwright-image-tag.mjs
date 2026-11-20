#!/usr/bin/env node
/**
 * The visual job runs Playwright inside `mcr.microsoft.com/playwright:vX-noble`
 * so snapshots are rasterised by one fixed font stack. That tag is written in
 * the workflow, while the library version lives in package.json — two places,
 * no link. Bumping `@playwright/test` without editing the workflow would leave
 * baselines being compared against a different browser build, which is the
 * quietest possible way to make a visual suite lie.
 *
 * This asserts the two agree, in the same spirit as gate-generated-client and
 * gate-route-wiring-map (issue #61, item 4).
 *
 * Run: node scripts/check-playwright-image-tag.mjs
 */
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(appRoot, '../..');
// EVERY workflow, not just ci-web.yml. This used to read ci-web.yml alone; when
// issue #157 moved the `visual` job to ci-web-e2e.yml the gate found zero
// references and reported "OK — visual suite pending", i.e. it silently stopped
// checking anything while still printing green. Scanning the directory means
// the gate follows the reference wherever the job lives.
const WORKFLOW_DIR = path.join(repoRoot, '.github/workflows');

const pkg = JSON.parse(readFileSync(path.join(appRoot, 'package.json'), 'utf8'));
const pinned = pkg.devDependencies?.['@playwright/test'] ?? pkg.dependencies?.['@playwright/test'];

if (typeof pinned !== 'string') {
  console.error('check-playwright-image-tag: @playwright/test is not a dependency of apps/elitea-web');
  process.exit(2);
}
if (!/^\d+\.\d+\.\d+$/.test(pinned)) {
  // An exact pin is the whole point: a range would make the image tag
  // unknowable at read time.
  console.error(`check-playwright-image-tag: @playwright/test must be pinned exactly, found "${pinned}"`);
  process.exit(2);
}

const refs = [];
for (const entry of readdirSync(WORKFLOW_DIR).sort()) {
  if (!/\.ya?ml$/.test(entry)) continue;
  const text = readFileSync(path.join(WORKFLOW_DIR, entry), 'utf8');
  for (const m of text.matchAll(/mcr\.microsoft\.com\/playwright:v(\d+\.\d+\.\d+)-noble/g)) {
    refs.push({ file: entry, version: m[1] });
  }
}

// The eng-runner image (deploy/ci-runners/image/Containerfile) bakes the
// browsers of one Playwright release for the native E2E jobs on the
// self-hosted pool. A library the browsers do not belong to refuses to launch
// them, so its `ARG PLAYWRIGHT_VERSION=` is held to the same pin. It is checked
// apart from `refs` below: the zero-references rule is about the visual suite's
// container, which this file is not.
const RUNNER_CONTAINERFILE = 'deploy/ci-runners/image/Containerfile';
const runnerContainerfile = path.join(repoRoot, RUNNER_CONTAINERFILE);
if (existsSync(runnerContainerfile)) {
  const text = readFileSync(runnerContainerfile, 'utf8');
  const args = [...text.matchAll(/^ARG PLAYWRIGHT_VERSION=(\S*)$/gm)].map((m) => m[1]);
  if (args.length !== 1) {
    console.error(`check-playwright-image-tag: FAIL — ${RUNNER_CONTAINERFILE} must declare exactly one`);
    console.error(`  'ARG PLAYWRIGHT_VERSION=<x.y.z>' line, found ${args.length}.`);
    process.exit(1);
  }
  if (args[0] !== pinned) {
    console.error('check-playwright-image-tag: FAIL — the runner image bakes another Playwright release');
    console.error(`  package.json @playwright/test: ${pinned}`);
    console.error(`  ${RUNNER_CONTAINERFILE} PLAYWRIGHT_VERSION: ${args[0]}`);
    console.error('  Bump the Containerfile ARG and PLAYWRIGHT_DIGEST with the library (its README, "Bumping").');
    process.exit(1);
  }
}

if (refs.length === 0) {
  // Zero references is only benign while there is no visual suite to pin. Once
  // e2e/visual exists, zero means the container reference was lost or renamed —
  // exactly the state the #157 split briefly produced — and reporting OK there
  // is how this gate stopped gating without anyone noticing.
  if (existsSync(path.join(appRoot, 'e2e/visual'))) {
    console.error('check-playwright-image-tag: FAIL — e2e/visual exists but no workflow references');
    console.error('  mcr.microsoft.com/playwright:v<x.y.z>-noble.');
    console.error('  The snapshot suite must run inside the pinned container; an unpinned run');
    console.error('  rasterises baselines with the runner\'s own font stack. Searched:');
    console.error(`  ${WORKFLOW_DIR}`);
    process.exit(1);
  }
  console.log('check-playwright-image-tag: OK — no Playwright container reference and no e2e/visual suite yet');
  process.exit(0);
}

const wrong = refs.filter((r) => r.version !== pinned);
if (wrong.length > 0) {
  console.error('check-playwright-image-tag: FAIL — container tag does not match the pinned library version');
  console.error(`  package.json @playwright/test: ${pinned}`);
  for (const r of wrong) console.error(`  ${r.file} references:${' '.repeat(Math.max(1, 22 - r.file.length))}${r.version}`);
  console.error('  Baselines rendered by one browser build and compared against another will diff for no real reason.');
  process.exit(1);
}

console.log(
  `check-playwright-image-tag: OK — ${refs.length} container reference(s) all pinned to v${pinned}-noble ` +
    `(${[...new Set(refs.map((r) => r.file))].join(', ')})`,
);
