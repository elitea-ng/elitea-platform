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
//     (`scripts/...` references in their YAML), and what each `.mjs` one
//     imports: its relative imports, transitively, and for a bare package
//     import the package.json and package-lock.json `npm ci` installs from,
//
// and fails, naming the path, when the `pull_request` filter covers none of
// them. (There is no `push` filter to keep equal to it: no workflow starts on
// a push, scripts/ci/check-no-push-triggers.mjs.) It
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

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";
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

// The module specifiers a script loads: its static imports and re-exports
// (`import x from "s"`, `import "s"`, `export { y } from "s"`), which ES
// modules hold at top level, so they are read from the start of a line, and
// its literal dynamic imports (`import("s")`) on any line that is not a
// comment. Prose that mentions an import is not a dependency.
const STATIC_IMPORT = /^(?:import|export)\s+(?:[^;'"]*?\sfrom\s*)?["']([^"'\n]+)["']/gm;
const DYNAMIC_IMPORT = /\bimport\s*\(\s*["']([^"'\n]+)["']\s*\)/g;

export function importSpecifiers(text) {
  const specs = [...text.matchAll(STATIC_IMPORT)].map((m) => m[1]);
  for (const line of splitlines(text)) {
    if (/^\s*(?:\/\/|\*|\/\*)/.test(line)) continue;
    for (const m of line.matchAll(DYNAMIC_IMPORT)) specs.push(m[1]);
  }
  return specs;
}

// The package.json that governs `dir`, walking up no further than `root`.
function governingManifest(dir, root) {
  for (let at = dir; at === root || at.startsWith(root + "/"); at = path.dirname(at)) {
    if (exists(path.join(at, "package.json"))) return at;
    if (at === root) break;
  }
  return null;
}

/**
 * Everything a referenced script needs besides itself, as a Map of
 * repository path -> the script that needs it: each `.mjs` follows its
 * relative imports transitively; a bare package import (not `node:`) makes
 * the governing package.json and package-lock.json inputs too, since they
 * pick the version `npm ci` installs.
 */
export function scriptDependencies(scripts, root = ROOT) {
  const deps = new Map();
  const seen = new Set();
  const queue = [...scripts].filter((s) => s.endsWith(".mjs")).map((s) => [s, s]);
  while (queue.length) {
    const [file, via] = queue.shift();
    if (seen.has(file)) continue;
    seen.add(file);
    const abs = path.join(root, file);
    if (!exists(abs)) continue;
    for (const spec of importSpecifiers(fs.readFileSync(abs, "utf8"))) {
      if (spec.startsWith("node:")) continue;
      if (spec.startsWith("./") || spec.startsWith("../")) {
        const dep = path.relative(root, path.resolve(path.dirname(abs), spec));
        if (dep.startsWith("..")) throw new Error(`${file} imports ${spec}, outside the repository`);
        if (!exists(path.join(root, dep))) throw new Error(`${file} imports ${spec}, and ${dep} does not exist`);
        if (!deps.has(dep) && !scripts.has(dep)) deps.set(dep, via);
        if (dep.endsWith(".mjs")) queue.push([dep, via]);
        continue;
      }
      if (spec.startsWith("/")) throw new Error(`${file} imports the absolute path ${spec}`);
      const manifest = governingManifest(path.dirname(abs), root);
      if (manifest === null) throw new Error(`${file} imports the package ${spec} and no package.json governs it`);
      for (const name of ["package.json", "package-lock.json"]) {
        const dep = path.relative(root, path.join(manifest, name));
        if (exists(path.join(root, dep)) && !deps.has(dep)) deps.set(dep, via);
      }
    }
  }
  return deps;
}

const ACTION_USE = /uses: \.\/(\.github\/actions\/[A-Za-z0-9_-]+)/g;
const sorted = (items) => [...items].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

export function main(log = console.log) {
  const workflowText = fs.readFileSync(WORKFLOW, "utf8");
  const workflow = loadYaml(workflowText);
  // PyYAML reads the unquoted `on:` key as True ("true" here).
  const on = Object.hasOwn(workflow, "true") ? workflow.true : workflow.on;
  const pr = [...on.pull_request.paths];
  const errors = [];
  if (pr.some((p) => p.startsWith("!"))) {
    errors.push("negated path patterns are not modelled by this check");
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
  const scripts = new Set();
  for (const [where, text] of texts) {
    for (const script of scriptReferences(text)) {
      if (exists(path.join(ROOT, script))) {
        setDefault(script, `called from ${where}`);
        scripts.add(script);
      }
    }
  }
  for (const [dep, script] of scriptDependencies(scripts)) setDefault(dep, `imported by ${script}`);

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

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
