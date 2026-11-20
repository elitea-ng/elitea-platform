#!/usr/bin/env node
// Fail a cache restore whose `path:` is not its writer's, character for character.
//
// WHY (measured 2026-11-20). main held node-cache-Linux-x64-npm-955ed20c… and a
// pull request that restored that exact key reported a MISS. An Actions cache
// entry has a key AND a version, and a restore matches only when both match.
// The version is (@actions/cache, getCacheVersion):
//
//     sha256( <path lines as written> | <compression> [| windows-only] | 1.0 )
//
// so it hashes the `path:` STRINGS, not the directories they name. The writer
// (.github/actions/main-write-cache) saved `~/.npm`, version c242501c…; the
// reader passed the path its caller had resolved, `/home/runner/.npm`, version
// 04d00cc6…. No pull request ever restored a Go or npm cache main wrote.
//
// WHAT IT CHECKS, over every actions/cache, actions/cache/restore and
// actions/cache/save step in .github/workflows/*.yml and .github/actions/**/action.yml:
//
//   1. `path:` is LITERAL. An expression resolves at run time to an absolute
//      path (a different string on Linux, macOS and Windows, and from a `~/`
//      writer), so its version cannot be known, let alone compared.
//   2. Every step of one key FAMILY spells the same path list, line for line
//      and in the same order (the hash is order-sensitive), and the same
//      `enableCrossOsArchive`. A family is the literal start of the key
//      (`go-v1-`, `node-cache-`); two families overlap when one starts with
//      the other, because restore-keys match by prefix. A key that is an
//      expression is followed to its literal start:
//        `${{ inputs.<x> }}` in a composite action: every caller's `<x>`, kept
//          to the callers the step's own `inputs.<y> == '<v>'` conditions admit
//          (main-write-cache's `slot`);
//        `${{ steps.<id>.outputs.<o> }}`: the `<o>` output of the local action
//          step `<id>` runs (go-cache's `key`).
//      A key that resolves to no literal is grouped with the steps of the same
//      file that read the same context (`steps.policy`), the closest thing to
//      "the same key" a static read can name.
//
// It fails, rather than passes, when it finds fewer cache steps than its floor,
// or when a required family (go-v1-, node-cache-) has no writer and no reader:
// "found nothing, therefore clean" is how a guard stops guarding in silence.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const MINIMUM_CACHE_STEPS = 6;
/** Families that must each have at least one saving and one restoring step. */
export const REQUIRED_FAMILIES = ["go-v1-", "node-cache-"];

const CACHE_ACTIONS = new Map([
  ["actions/cache", { saves: true, restores: true }],
  ["actions/cache/restore", { saves: false, restores: true }],
  ["actions/cache/save", { saves: true, restores: false }],
]);

const isDict = (v) => v !== null && typeof v === "object" && !Array.isArray(v) && !(v instanceof Date);
const str = (v) => (v === null || v === undefined ? "" : String(v));
const get = (obj, key) => (isDict(obj) && Object.hasOwn(obj, key) ? obj[key] : undefined);

/** The path lines exactly as actions/cache reads them: split on newlines, trimmed, empty lines dropped. */
export function pathLines(value) {
  return str(value)
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

/** The literal start of a key: the text before its first expression. */
const literalStart = (key) => str(key).split("${{", 1)[0].trim();

const INPUT_REF = /^\$\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*\}\}/;
const STEP_OUTPUT_REF = /^\$\{\{\s*steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*\}\}/;
const CONTEXT_REF = /\$\{\{\s*([A-Za-z_]+\.[A-Za-z0-9_-]+)/;
const INPUT_EQUALS = /inputs\.([A-Za-z0-9_-]+)\s*==\s*['"]([^'"]*)['"]/g;

/** Every [job, steps[]] of a workflow; ["<action>", steps[]] for a composite. */
function stepLists(doc) {
  if (isDict(get(doc, "runs"))) return [["<action>", Array.isArray(doc.runs.steps) ? doc.runs.steps : []]];
  const jobs = get(doc, "jobs");
  if (!isDict(jobs)) return [];
  return Object.entries(jobs).map(([id, job]) => [id, Array.isArray(get(job, "steps")) ? job.steps : []]);
}

const actionOf = (step) => str(get(step, "uses")).split("@", 1)[0].trim();

/** `./.github/actions/x` → `.github/actions/x/action.yml` (or .yaml), when that document exists. */
function localAction(docs, uses) {
  if (!uses.startsWith("./")) return null;
  const dir = uses.slice(2).replace(/\/+$/, "");
  for (const name of [`${dir}/action.yml`, `${dir}/action.yaml`]) if (docs.has(name)) return name;
  return null;
}

/**
 * The literal key starts an expression key resolves to, or null when it
 * resolves to none. `conds` are `inputs.<y> == '<v>'` pairs the step that
 * uses this key runs under, applied to the callers of a composite.
 */
function resolveKey(docs, file, steps, key, conds, depth = 0) {
  const literal = literalStart(key);
  if (literal !== "") return new Set([literal]);
  if (depth > 4) return null;
  const text = str(key).trim();
  const input = INPUT_REF.exec(text);
  if (input && isDict(get(docs.get(file), "runs"))) {
    const out = new Set();
    for (const [callerFile, doc] of docs) {
      for (const [, callerSteps] of stepLists(doc)) {
        for (const step of callerSteps) {
          if (!isDict(step) || localAction(docs, actionOf(step)) !== file) continue;
          const with_ = get(step, "with") ?? {};
          if (conds.some(([name, value]) => str(get(with_, name)).trim() !== value)) continue;
          const resolved = resolveKey(docs, callerFile, callerSteps, get(with_, input[1]), [], depth + 1);
          if (resolved === null) return null;
          for (const k of resolved) out.add(k);
        }
      }
    }
    return out.size > 0 ? out : null;
  }
  const output = STEP_OUTPUT_REF.exec(text);
  if (output) {
    const source = steps.find((s) => isDict(s) && str(get(s, "id")) === output[1]);
    const action = source ? localAction(docs, actionOf(source)) : null;
    const value = action ? get(get(get(docs.get(action), "outputs"), output[2]), "value") : undefined;
    if (value !== undefined) return resolveKey(docs, action, [], value, [], depth + 1);
  }
  return null;
}

/** Every cache step: {where, file, family[], saves, restores, paths, crossOs, expressionPath}. */
export function collect(docs) {
  const out = [];
  for (const [file, doc] of docs) {
    for (const [job, steps] of stepLists(doc)) {
      for (const step of steps) {
        if (!isDict(step)) continue;
        const kind = CACHE_ACTIONS.get(actionOf(step));
        if (!kind) continue;
        const with_ = get(step, "with") ?? {};
        const conds = [...str(get(step, "if")).matchAll(INPUT_EQUALS)].map((m) => [m[1], m[2]]);
        const key = get(with_, "key");
        const resolved = resolveKey(docs, file, steps, key, conds);
        const context = CONTEXT_REF.exec(str(key))?.[1] ?? str(key).trim();
        const label = str(get(step, "name")) || str(get(step, "id")) || actionOf(step);
        out.push({
          where: `${file} [${job}] '${label}'`,
          file,
          families: resolved ? [...resolved].sort() : [`${file}#${context}`],
          ...kind,
          paths: pathLines(get(with_, "path")),
          crossOs: str(get(with_, "enableCrossOsArchive")).trim().toLowerCase() === "true",
          expressionPath: str(get(with_, "path")).includes("${{"),
        });
      }
    }
  }
  return out;
}

const overlaps = (a, b) => a.startsWith(b) || b.startsWith(a);
const shareFamily = (x, y) => x.families.some((a) => y.families.some((b) => overlaps(a, b)));
const spelled = (s) => `${s.paths.map((p) => JSON.stringify(p)).join(" | ") || "(none)"}${s.crossOs ? " enableCrossOsArchive" : ""}`;

/** {errors[], steps} for a map of repository-relative file name → parsed document. */
export function check(docs, { requiredFamilies = REQUIRED_FAMILIES } = {}) {
  const steps = collect(docs);
  const errors = [];
  for (const s of steps) {
    if (s.expressionPath) {
      errors.push(`${s.where}: \`path:\` is an expression. The entry's version is a hash of the path STRINGS as written, and an expression resolves to an absolute path that no \`~/\` writer (and no other OS) spells the same way, so the restore misses. Write the writer's literal paths.`);
    }
  }
  const reported = new Set();
  for (let i = 0; i < steps.length; i += 1) {
    for (let j = i + 1; j < steps.length; j += 1) {
      const [a, b] = [steps[i], steps[j]];
      if (a.expressionPath || b.expressionPath || !shareFamily(a, b) || spelled(a) === spelled(b)) continue;
      const pair = [a.where, b.where].sort().join("\n");
      if (reported.has(pair)) continue;
      reported.add(pair);
      errors.push(`key family ${a.families.join(", ")}: ${a.where} spells ${spelled(a)} but ${b.where} spells ${spelled(b)}. The cache version is a hash of these exact strings, so one never restores what the other saved; spell them identically (the literal \`~/...\` form).`);
    }
  }
  for (const family of requiredFamilies) {
    const members = steps.filter((s) => s.families.some((f) => overlaps(f, family)));
    if (!members.some((s) => s.saves) || !members.some((s) => s.restores)) {
      errors.push(`key family ${family}: found ${members.filter((s) => s.saves).length} saving and ${members.filter((s) => s.restores).length} restoring step(s), expected at least one of each — the key resolution stopped working, so this check compared nothing for it`);
    }
  }
  return { errors, steps };
}

function walk(dir, accept) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  return entries
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .flatMap((e) => (e.isDirectory() ? walk(path.join(dir, e.name), accept) : accept(e.name) ? [path.join(dir, e.name)] : []));
}

/** Every workflow and composite action under root, parsed, keyed by its repository-relative path. */
export function load(root) {
  const files = [
    ...walk(path.join(root, ".github", "workflows"), (n) => /\.ya?ml$/.test(n)),
    ...walk(path.join(root, ".github", "actions"), (n) => n === "action.yml" || n === "action.yaml"),
  ];
  const docs = new Map();
  for (const file of files) {
    const doc = loadYaml(fs.readFileSync(file, "utf8"));
    docs.set(path.relative(root, file).split(path.sep).join("/"), isDict(doc) ? doc : {});
  }
  return docs;
}

export function main(argv = process.argv.slice(2), log = console.log) {
  let values;
  try {
    ({ values } = parseArgs({ args: argv, strict: true, options: { root: { type: "string" }, "min-cache-steps": { type: "string" } } }));
  } catch (error) {
    log(`usage: check-cache-path-parity.mjs [--root ROOT] [--min-cache-steps N]\n${error.message}`);
    return 2;
  }
  const minimum = values["min-cache-steps"] === undefined ? MINIMUM_CACHE_STEPS : Number.parseInt(values["min-cache-steps"], 10);
  const { errors, steps } = check(load(path.resolve(values.root ?? REPOSITORY_ROOT)));
  if (steps.length < minimum) {
    log(`::error::found ${steps.length} actions/cache step(s), expected at least ${minimum} — the step matching stopped working, so this check compared nothing`);
    return 1;
  }
  for (const error of errors) log(`::error::${error}`);
  if (errors.length > 0) {
    log(`${errors.length} cache path mismatch(es)`);
    return 1;
  }
  const families = new Set(steps.flatMap((s) => s.families));
  log(`cache path parity: ${steps.length} actions/cache step(s) in ${families.size} key famil${families.size === 1 ? "y" : "ies"}; every restore spells its writer's paths`);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
