#!/usr/bin/env node
// Build once, pull everywhere: the stack-image digest map for ci-web-e2e.yml.
//
// ci-web-e2e.yml builds each stack image ONCE per commit (its `*-images` jobs,
// through .github/actions/build-stack-images in push mode) and every journey
// lane pulls the result by digest (.github/actions/pull-stack-images). This
// script is the contract between the two halves, so neither re-derives it;
// USAGE below is what it prints for a call it does not understand.
//
// `docker compose config --format json` produces <config.json>. Its build
// contexts are absolute paths; they are compared relative to <workspace>, so a
// build job and a lane on different runners agree.
//
// Run with the runner's node; only the standard library.
// scripts/ci/stack-images.test.mjs holds the contract's scenarios.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr } from "./py-compat.mjs";

export const USAGE = `Build once, pull everywhere: the stack-image digest map for ci-web-e2e.yml.

ci-web-e2e.yml builds each stack image ONCE per commit (its \`*-images\` jobs,
through .github/actions/build-stack-images in push mode) and every journey
lane pulls the result by digest (.github/actions/pull-stack-images). This
script is the contract between the two halves, so neither re-derives it:

  plan     <config.json> <workspace> <prefix> <tag> <plan-out> <service>...
           For each compose SERVICE to build: the CI-only image name
           (<prefix><image>, <image> the last path segment of the service's
           \`image:\`), its build spec, and a bake \`--set <service>.tags=...\`
           line on stdout.
  record   <plan.json> <bake-metadata.json>
           The digest map fragment for one build job, on stdout:
           {<image>: {"ref": ..., "digest": "sha256:...", "build": {...}}}.
  resolve  <config.json> <workspace> <digests-dir>
           For a lane's compose config: one \`<local tag>\\t<ref>@<digest>\`
           line per image compose would build. Fails, naming the image, when
           the map has no entry for it or the entry was built from a
           DIFFERENT spec (context, Containerfile, target, args) — a lane
           whose compose names a variant nobody built must not quietly run
           another one.

\`docker compose config --format json\` produces <config.json>. Its build
contexts are absolute paths; they are compared relative to <workspace>, so a
build job and a lane on different runners agree.
`;

export class StackImageError extends Error {}

/** `ghcr.io/org/elitea-main:standalone` → `elitea-main`. */
export function imageName(image) {
  const last = image.split("/").pop();
  return last.split("@", 1)[0].split(":", 1)[0];
}

// pathlib.PurePosixPath's normal form: no `.` segments, no repeated or
// trailing slash, "." for nothing.
function purePosix(text) {
  const absolute = text.startsWith("/");
  const parts = text.split("/").filter((part) => part !== "" && part !== ".");
  const joined = parts.join("/");
  if (absolute) return "/" + joined;
  return joined || ".";
}

function relativeTo(child, root) {
  const c = child === "/" ? [] : child.slice(1).split("/");
  const r = root === "/" ? [] : root.slice(1).split("/");
  if (r.length > c.length || r.some((part, i) => c[i] !== part)) return null;
  return c.slice(r.length).join("/") || ".";
}

function sortedEntries(object) {
  return Object.entries(object || {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

export function buildSpec(build, workspace) {
  const context = purePosix(build.context || ".");
  const root = purePosix(workspace);
  let relative = context;
  if (context.startsWith("/")) {
    relative = root.startsWith("/") ? relativeTo(context, root) : null;
    if (relative === null) {
      throw new StackImageError(`build context ${context} is outside the workspace ${workspace}`);
    }
  }
  return {
    context: relative,
    dockerfile: build.dockerfile || "Dockerfile",
    target: build.target || "",
    args: Object.fromEntries(sortedEntries(build.args)),
  };
}

/** service → [image tag, build spec], for every service with `build:`. */
export function builtServices(config, workspace) {
  const out = new Map();
  for (const [name, service] of sortedEntries(config.services)) {
    const build = service.build;
    if (!build || (typeof build === "object" && Object.keys(build).length === 0)) continue;
    const image = service.image;
    if (!image) {
      throw new StackImageError(`service ${name} builds an image but declares no \`image:\` to tag it with`);
    }
    out.set(name, [image, buildSpec(build, workspace)]);
  }
  return out;
}

export function plan(config, workspace, prefix, tag, services) {
  const available = builtServices(config, workspace);
  const entries = [];
  const sets = [];
  const seen = new Map();
  for (const service of services) {
    if (!available.has(service)) {
      throw new StackImageError(`${service} is not a \`build:\` service of these compose files`);
    }
    const [image, spec] = available.get(service);
    const name = imageName(image);
    if (seen.has(name)) {
      throw new StackImageError(`${service} and ${seen.get(name)} are the same image (${name}); build it once`);
    }
    seen.set(name, service);
    const ref = `${prefix}${name}`;
    entries.push({ service, image: name, ref, tag, build: spec });
    sets.push(`${service}.tags=${ref}:${tag}`);
  }
  return [entries, sets];
}

export function record(entries, metadata) {
  const out = {};
  for (const entry of entries) {
    const digest = (metadata[entry.service] || {})["containerimage.digest"] ?? "";
    if (!digest.startsWith("sha256:")) {
      throw new StackImageError(`bake reported no pushed digest for ${entry.service} (${entry.image})`);
    }
    out[entry.image] = { ref: entry.ref, digest, build: entry.build };
  }
  return out;
}

/** Python's == over JSON values: key order does not matter. */
export function deepEqual(a, b) {
  if (a === b) return true;
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  if (Array.isArray(a)) return a.length === b.length && a.every((x, i) => deepEqual(x, b[i]));
  const ka = Object.keys(a);
  const kb = Object.keys(b);
  return ka.length === kb.length && ka.every((k) => Object.hasOwn(b, k) && deepEqual(a[k], b[k]));
}

export function merge(fragments) {
  const merged = {};
  for (const [source, fragment] of fragments) {
    for (const [name, entry] of Object.entries(fragment)) {
      if (Object.hasOwn(merged, name) && !deepEqual(merged[name], entry)) {
        throw new StackImageError(`${name} appears twice in the digest map with different contents (${source})`);
      }
      merged[name] = entry;
    }
  }
  return merged;
}

export function resolve(config, workspace, digests) {
  const pulls = new Map();
  const problems = [];
  for (const [service, [image, spec]] of builtServices(config, workspace)) {
    const name = imageName(image);
    const entry = Object.hasOwn(digests, name) ? digests[name] : undefined;
    if (entry === undefined || entry === null) {
      problems.push(`${image} (service ${service}): no image named ${name} was built for this commit`);
      continue;
    }
    if (!deepEqual(entry.build, spec)) {
      problems.push(`${image} (service ${service}): built from ${pyRepr(entry.build)}, but this compose builds ${pyRepr(spec)}`);
      continue;
    }
    if (!pulls.has(image)) pulls.set(image, `${entry.ref}@${entry.digest}`);
  }
  if (problems.length) throw new StackImageError(problems.join("; "));
  if (pulls.size === 0) {
    throw new StackImageError("the compose files declare no build: services — pulling nothing would be vacuous");
  }
  return [...pulls.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

// json.dumps(value, indent=2, sort_keys=True): sorted keys, ASCII only.
function sortKeys(value) {
  if (Array.isArray(value)) return value.map(sortKeys);
  if (value && typeof value === "object") return Object.fromEntries(sortedEntries(value).map(([k, v]) => [k, sortKeys(v)]));
  return value;
}

export function dumps(value) {
  return JSON.stringify(sortKeys(value), null, 2).replace(/[\u007f-￿]/g, (ch) => "\\u" + ch.charCodeAt(0).toString(16).padStart(4, "0"));
}

function load(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

export function main(argv) {
  try {
    if (argv.length >= 8 && argv[1] === "plan") {
      const [entries, sets] = plan(load(argv[2]), argv[3], argv[4], argv[5], argv.slice(7));
      fs.writeFileSync(argv[6], dumps(entries));
      process.stdout.write(sets.join("\n") + "\n");
    } else if (argv.length === 4 && argv[1] === "record") {
      process.stdout.write(dumps(record(load(argv[2]), load(argv[3]))) + "\n");
    } else if (argv.length === 5 && argv[1] === "resolve") {
      const dir = argv[4];
      let names = [];
      try {
        names = fs.readdirSync(dir);
      } catch {
        names = [];
      }
      const files = names.filter((n) => n.endsWith(".json")).map((n) => path.join(dir, n)).sort();
      if (files.length === 0) throw new StackImageError(`no digest files in ${dir}`);
      const digests = merge(files.map((f) => [f, load(f)]));
      for (const [local, remote] of resolve(load(argv[2]), argv[3], digests)) {
        process.stdout.write(`${local}\t${remote}\n`);
      }
    } else {
      process.stderr.write(USAGE + "\n");
      return 2;
    }
  } catch (error) {
    if (error instanceof StackImageError) {
      process.stderr.write(`::error::${error.message}\n`);
      return 1;
    }
    throw error;
  }
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(1));
}
