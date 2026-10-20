#!/usr/bin/env node
// ci-web-e2e.yml starts on every change that can change what its lanes run.
//
// The workflow is path-filtered, and a path filter is a list someone has to
// keep complete: a change to an input it does not name never starts the lanes
// that would have caught it, and the checks list goes green by absence. This
// derives the inputs instead of trusting the list:
//
//   * every `build:` service of the compose files the lanes run: its
//     Containerfile and every COPY source (from the build context), and
//   * every repository script the workflow or the local actions it uses call
//     (`scripts/...` references in their YAML),
//
// and fails, naming the path, when the `pull_request` filter covers none of
// them — or when the `push` filter differs from the `pull_request` one. It
// also runs scripts/ci/e2e-lane-filter.sh (the `model-path-changes` job's
// skip rule) over each input and fails on any it would call outside the stack:
// the two lists must agree that a stack input starts the stack lanes.
//
// Patterns are matched the way GitHub matches them for this list's shapes:
// `dir/**` is everything under dir/, anything else is an exact path or an
// fnmatch glob. A negated pattern (`!x`) is not used here and is refused, so
// the matcher never has to model one.
//
// Run with the runner's node after `npm ci --prefix scripts/ci` (the `yaml`
// package). Compose's merge tags (`!reset`, `!override`) carry no meaning for
// this read; ./ci-compat.mjs reads them as untagged.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml } from "./ci-compat.mjs";
import { fnmatchcase, pyRepr, splitlines } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");
const WORKFLOW = path.join(ROOT, ".github", "workflows", "ci-web-e2e.yml");
export const COMPOSE_FILES = [
  "deploy/docker-compose.e2e-standalone.yml",
  "deploy/docker-compose.standalone-full.yml",
  "deploy/docker-compose.standalone-rust-agent.yml",
];
// Floors: a parser that stopped finding things must not pass.
export const MINIMUM_BUILD_SERVICES = 15;
export const MINIMUM_INPUTS = 40;

const isDir = (p) => {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
};
const exists = (p) => fs.existsSync(p);

// pathlib's `a / b`: an absolute b replaces a; `.` and empty segments drop,
// `..` stays as written.
function join(base, rel) {
  const joined = rel.startsWith("/") ? rel : `${base}/${rel}`;
  const parts = joined.split("/").filter((p) => p !== "" && p !== ".");
  return "/" + parts.join("/");
}

// Path.resolve(): absolute, `..` folded, symlinks followed where they exist.
function resolve(p) {
  const plain = path.resolve(p);
  try {
    return fs.realpathSync(plain);
  } catch {
    return plain;
  }
}

// Path.relative_to(ROOT): a prefix of whole segments, or a ValueError.
function relativeToRoot(p) {
  if (p === ROOT) return ".";
  if (!p.startsWith(ROOT + "/")) throw new Error(`'${p}' is not in the subpath of '${ROOT}'`);
  return p.slice(ROOT.length + 1);
}

export function covered(file, patterns, root = ROOT) {
  // A COPY of a directory is covered only by a pattern that takes the whole
  // directory: `dir/**` itself or one of its parents'.
  const probe = isDir(path.join(root, file)) ? file + "/" : file;
  for (const pattern of patterns) {
    if (pattern.endsWith("/**")) {
      if (probe.startsWith(pattern.slice(0, -2))) return true;
    } else if (file === pattern || fnmatchcase(file, pattern)) {
      return true;
    }
  }
  return false;
}

export function containerfileInputs(context, dockerfile) {
  const inputs = new Set([relativeToRoot(dockerfile)]);
  const text = fs.readFileSync(dockerfile, "utf8");
  const joined = text.replace(/\\\n/g, " ");
  for (const line of splitlines(joined)) {
    const words = line.split(/\s+/).filter(Boolean);
    if (!words.length || !["COPY", "ADD"].includes(words[0].toUpperCase())) continue;
    const args = words.slice(1).filter((w) => !w.startsWith("--"));
    if (words.slice(1).some((w) => w.startsWith("--from")) || args.some((a) => a.startsWith("<<"))) continue;
    for (const source of args.slice(0, -1)) {
      if (source.includes("://")) continue; // ADD of a URL: not a repository input
      inputs.add(relativeToRoot(resolve(join(context, source))));
    }
  }
  return inputs;
}

export function scriptReferences(text) {
  return new Set([...text.matchAll(/\b(scripts\/[A-Za-z0-9_./-]+\.(?:sh|py|mjs))\b/g)].map((m) => m[1]));
}

const ACTION_USE = /uses: \.\/(\.github\/actions\/[A-Za-z0-9_-]+)/g;
const sorted = (items) => [...items].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

export function main(log = console.log) {
  const workflowText = fs.readFileSync(WORKFLOW, "utf8");
  const workflow = loadYaml(workflowText);
  // PyYAML reads the unquoted `on:` key as True ("true" here).
  const on = Object.hasOwn(workflow, "true") ? workflow.true : workflow.on;
  const pr = [...on.pull_request.paths];
  const push = [...on.push.paths];
  const errors = [];
  if ([...pr, ...push].some((p) => p.startsWith("!"))) {
    errors.push("negated path patterns are not modelled by this check");
  }
  if (JSON.stringify(sorted(pr)) !== JSON.stringify(sorted(push))) {
    const onlyPr = sorted(new Set(pr.filter((p) => !push.includes(p))));
    const onlyPush = sorted(new Set(push.filter((p) => !pr.includes(p))));
    errors.push(`the push paths differ from the pull_request paths: only in pull_request ${pyRepr(onlyPr)}, only in push ${pyRepr(onlyPush)}`);
  }

  const inputs = new Map();
  const setDefault = (key, value) => {
    if (!inputs.has(key)) inputs.set(key, value);
  };
  let services = 0;
  for (const compose of COMPOSE_FILES) {
    const doc = loadYaml(fs.readFileSync(path.join(ROOT, compose), "utf8")) || {};
    for (const [name, service] of Object.entries(doc.services || {})) {
      const build = (service || {}).build;
      if (!build || typeof build !== "object" || Array.isArray(build)) continue;
      services += 1;
      const context = resolve(join(path.join(ROOT, "deploy"), String(build.context ?? ".")));
      const dockerfile = join(context, String(build.dockerfile ?? "Dockerfile"));
      for (const file of containerfileInputs(context, dockerfile)) {
        setDefault(file, `${compose} service ${name}`);
      }
    }
  }
  const texts = new Map([[relativeToRoot(WORKFLOW), workflowText]]);
  for (const [, action] of workflowText.matchAll(ACTION_USE)) {
    const file = path.join(ROOT, action, "action.yml");
    const text = fs.readFileSync(file, "utf8");
    texts.set(relativeToRoot(file), text);
    for (const [, nested] of text.matchAll(ACTION_USE)) {
      texts.set(`${nested}/action.yml`, fs.readFileSync(path.join(ROOT, nested, "action.yml"), "utf8"));
    }
  }
  for (const [where, text] of texts) {
    for (const script of scriptReferences(text)) {
      if (exists(path.join(ROOT, script))) setDefault(script, `called from ${where}`);
    }
  }

  if (services < MINIMUM_BUILD_SERVICES || inputs.size < MINIMUM_INPUTS) {
    errors.push(`found ${services} build service(s) and ${inputs.size} input(s), below the floors ${MINIMUM_BUILD_SERVICES}/${MINIMUM_INPUTS}: the reader stopped finding things`);
  }
  const ordered = sorted(inputs.keys()).map((k) => [k, inputs.get(k)]);
  for (const [file, why] of ordered) {
    if (!covered(file, pr)) errors.push(`${file} (${why}) is an input of the E2E lanes and no paths pattern covers it`);
  }
  const laneFilter = path.join(ROOT, "scripts", "ci", "e2e-lane-filter.sh");
  for (const [file, why] of ordered) {
    // A directory input stands for any file under it; probe with one the
    // filter cannot treat as prose.
    const probe = isDir(path.join(ROOT, file)) ? file + "/probe.rs" : file;
    const out = spawnSync("bash", [laneFilter], { input: probe + "\n", cwd: ROOT, encoding: "utf8" }).stdout || "";
    if (!out.includes("reaches-stack=true")) {
      errors.push(`${file} (${why}) is an input of the E2E lanes, but e2e-lane-filter.sh says a change to it skips them: ${pyRepr(out.trim())}`);
    }
  }
  for (const error of errors) log(`::error::${error}`);
  if (errors.length) return 1;
  log(`ci-web-e2e.yml paths: ${inputs.size} input(s) of ${services} build service(s) and the scripts, all covered by ${pr.length} pattern(s)`);
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === SELF) {
  process.exitCode = main();
}
