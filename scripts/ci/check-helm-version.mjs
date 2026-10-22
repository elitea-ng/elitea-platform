#!/usr/bin/env node
// One Helm version in CI, and a Helm 3 render beside it.
//
// THE PINS (below, and nowhere else):
//
//   HELM_VERSION   every `azure/setup-helm` step in .github/workflows/*.yml and
//                  .github/actions/*/action.yml installs exactly this. Lint,
//                  render, the kind install and the release package all use
//                  one Helm, so the chart a release ships is the chart CI
//                  rendered.
//   HELM3_VERSION  the latest Helm 3 patch. Argo CD renders charts with the
//                  Helm 3 it bundles, so the chart must keep rendering with
//                  Helm 3. Exactly ONE setup-helm step installs it: the Helm 3
//                  render in helm-lint.yml (`id: helm3`).
//
// WHY A GATE. Dependabot's github-actions ecosystem bumps `azure/setup-helm@v5`
// but never reads its `version:` input, so a new Helm release changes nothing
// in this repository by itself, and eight copies of a literal drift apart one
// edit at a time. This check holds the copies to the pins; `--latest` (run
// daily by dependency-scanning.yml, which has network) fails when a pin is
// behind the newest Helm 4 or Helm 3 release, so the pin is never left behind
// in silence. To raise a pin: change it here and in every step this names.
//
// It fails, rather than passes, when it finds fewer setup-helm steps than
// MIN_STEPS: "found nothing, therefore consistent" is how a guard stops
// guarding. Runs with the runner's node and the `yaml` package
// (scripts/ci/package.json).

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

export const HELM_VERSION = "v4.3.1";
export const HELM3_VERSION = "v3.22.1";
export const MIN_STEPS = 8;
const HELM3_FILE = ".github/workflows/helm-lint.yml";
const HELM3_STEP_ID = "helm3";

function files(root) {
  const out = [];
  const wf = path.join(root, ".github", "workflows");
  if (fs.existsSync(wf)) {
    for (const f of fs.readdirSync(wf).sort()) if (/\.ya?ml$/.test(f)) out.push(path.join(".github", "workflows", f));
  }
  const actions = path.join(root, ".github", "actions");
  if (fs.existsSync(actions)) {
    for (const d of fs.readdirSync(actions).sort()) {
      for (const f of ["action.yml", "action.yaml"]) {
        if (fs.existsSync(path.join(actions, d, f))) out.push(path.join(".github", "actions", d, f));
      }
    }
  }
  return out;
}

function stepsOf(doc) {
  const steps = [];
  for (const job of Object.values(doc?.jobs ?? {})) for (const s of job?.steps ?? []) steps.push(s);
  for (const s of doc?.runs?.steps ?? []) steps.push(s);
  return steps;
}

/** Every setup-helm step: {file, id, version}. */
export function setupHelmSteps(root) {
  const found = [];
  for (const file of files(root)) {
    const doc = parse(fs.readFileSync(path.join(root, file), "utf8"));
    for (const step of stepsOf(doc)) {
      if (typeof step?.uses !== "string" || !step.uses.startsWith("azure/setup-helm@")) continue;
      found.push({ file, id: step.id ?? "", version: String(step.with?.version ?? "") });
    }
  }
  return found;
}

export function check(root) {
  const problems = [];
  const steps = setupHelmSteps(root);
  if (steps.length < MIN_STEPS) {
    problems.push(`found ${steps.length} azure/setup-helm step(s), expected at least ${MIN_STEPS}; the scan stopped matching`);
  }
  let helm3 = 0;
  for (const s of steps) {
    const where = `${s.file}${s.id ? ` (id: ${s.id})` : ""}`;
    if (s.file === HELM3_FILE && s.id === HELM3_STEP_ID) {
      helm3 += 1;
      if (s.version !== HELM3_VERSION) problems.push(`${where}: installs Helm '${s.version}', the Helm 3 pin is ${HELM3_VERSION}`);
    } else if (s.version !== HELM_VERSION) {
      problems.push(`${where}: installs Helm '${s.version || "(no version: the runner's)"}', the pin is ${HELM_VERSION}`);
    }
  }
  if (helm3 !== 1) {
    problems.push(`${HELM3_FILE}: expected exactly one setup-helm step with id ${HELM3_STEP_ID} (the Helm 3 render), found ${helm3}`);
  }
  return { steps, problems };
}

const semver = (v) => v.replace(/^v/, "").split(".").map((n) => parseInt(n, 10));
export function behind(pin, latest) {
  const a = semver(pin);
  const b = semver(latest);
  for (let i = 0; i < 3; i += 1) if (a[i] !== b[i]) return a[i] < b[i];
  return false;
}

async function latestReleases() {
  const headers = { Accept: "application/vnd.github+json" };
  if (process.env.GH_TOKEN) headers.Authorization = `Bearer ${process.env.GH_TOKEN}`;
  const res = await fetch("https://api.github.com/repos/helm/helm/releases?per_page=100", { headers });
  if (!res.ok) throw new Error(`GitHub API answered ${res.status}`);
  const tags = (await res.json()).filter((r) => !r.prerelease && !r.draft).map((r) => r.tag_name)
    .filter((t) => /^v\d+\.\d+\.\d+$/.test(t));
  const newest = (major) => tags.filter((t) => t.startsWith(`v${major}.`)).sort((x, y) => (behind(x, y) ? 1 : -1))[0];
  return { v4: newest(4), v3: newest(3) };
}

async function main() {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
  const { steps, problems } = check(root);
  for (const p of problems) process.stderr.write(`ERROR: ${p}\n`);
  if (process.argv.includes("--latest")) {
    const { v4, v3 } = await latestReleases();
    const late = [];
    if (!v4 || !v3) late.push("read no Helm 4 or Helm 3 release from GitHub");
    else {
      process.stdout.write(`latest: Helm ${v4}, Helm ${v3}\n`);
      if (behind(HELM_VERSION, v4)) late.push(`Helm ${v4} is out; the pin is ${HELM_VERSION}. Raise HELM_VERSION in scripts/ci/check-helm-version.mjs and every setup-helm step.`);
      if (behind(HELM3_VERSION, v3)) late.push(`Helm ${v3} is out; the Helm 3 pin is ${HELM3_VERSION}. Raise HELM3_VERSION and helm-lint.yml's helm3 step.`);
    }
    for (const p of late) process.stderr.write(`ERROR: ${p}\n`);
    problems.push(...late);
  }
  if (problems.length) process.exit(1);
  process.stdout.write(`helm pins: ${steps.length} setup-helm step(s), ${HELM_VERSION} everywhere and ${HELM3_VERSION} for the Helm 3 render\n`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
