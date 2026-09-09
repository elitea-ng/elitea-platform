#!/usr/bin/env node
/**
 * docs-shots-merge.mjs — folds content writers' staging shot files into
 * `src/entries/docs/shots.manifest.ts` (embedded-docs programme, unit W3).
 *
 * Writers author MDX in a scratchpad staging directory, not this worktree,
 * and drop one `shots.<batch>.json` per batch there: a JSON array of
 * `{ id, route, viewport?, selector?, actions?, mask?, persona?, notes? }`
 * (the same shape as `shots.manifest.ts`'s `Shot`, minus `theme` — writers
 * do not choose colour scheme, decision 4 pins every shot to light).
 *
 * Usage:
 *   node scripts/docs-shots-merge.mjs <staging-dir>
 *
 * Reads every `shots.*.json` in <staging-dir> (sorted by filename, so
 * `shots.A.json` folds in before `shots.B.json`), merges them with whatever
 * is already in `shots.manifest.ts`, dedupes by `id`, and rewrites the
 * `shots` array in place. An `id` that appears twice with DIFFERENT content
 * is an error — the merge writes nothing in that case, so a bad batch can
 * never partially land. Re-running it once the conflicting batch is fixed is
 * always safe: identical duplicates are silently folded into one entry.
 *
 * The file's type declarations (`Shot`, `ShotAction`, `ShotPersona`,
 * `DEFAULT_VIEWPORT`) and its header comment are left untouched — only the
 * `export const shots: readonly Shot[] = [...]` block is replaced. The
 * replacement is then run through `prettier --write` (already a
 * devDependency here) so hand-rolled serialisation never has to match the
 * project's formatting rules by hand.
 */
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = resolve(SCRIPT_DIR, "..");
const MANIFEST_PATH = join(WEB_ROOT, "src/entries/docs/shots.manifest.ts");
const DEFAULT_VIEWPORT = { width: 1440, height: 900 };

const REQUIRED_FIELDS = ["id", "route"];
const KNOWN_FIELDS = new Set([
  "id",
  "route",
  "viewport",
  "selector",
  "actions",
  "mask",
  "persona",
  "theme",
  "notes",
  "seed",
  "placeholders",
]);

function fail(message) {
  console.error(`docs-shots-merge: ${message}`);
  process.exit(1);
}

function readStagingDir(dir) {
  if (!existsSync(dir)) fail(`staging directory does not exist: ${dir}`);
  const files = readdirSync(dir)
    .filter((name) => /^shots\..+\.json$/.test(name))
    .sort();
  if (files.length === 0) {
    console.warn(
      `docs-shots-merge: no shots.*.json files in ${dir} — nothing to fold in.`,
    );
  }
  return files.map((name) => ({ name, path: join(dir, name) }));
}

/** Writers spell a viewport as `"1440x900"`; the manifest type wants
 * `{ width, height }`. Accept both so a staging file and a hand-edited
 * manifest entry can describe the same shot without a conflict. */
function normalizeViewport(viewport, source, id) {
  if (viewport === undefined) return DEFAULT_VIEWPORT;
  if (typeof viewport === "object" && viewport !== null) {
    const { width, height } = viewport;
    if (typeof width === "number" && typeof height === "number")
      return { width, height };
    fail(
      `${source}: entry "${id}" has a viewport object without numeric width/height`,
    );
  }
  if (typeof viewport === "string") {
    const match = /^(\d+)x(\d+)$/.exec(viewport);
    if (match) return { width: Number(match[1]), height: Number(match[2]) };
  }
  fail(
    `${source}: entry "${id}" has an unparseable viewport (want {width,height} or "WxH"): ${JSON.stringify(viewport)}`,
  );
  return DEFAULT_VIEWPORT;
}

/**
 * The main SPA is served under the `/app` basepath on every stack this
 * script targets (`vite_base_uri` defaults to `/app/`; confirmed against
 * `e2e/journeys/**` — every journey that navigates directly builds
 * `BASE_URL + '/app/...'`, never a bare `/...`). The admin console is a
 * SEPARATE bundle mounted at `/admin/app` (`src/pages/admin/router.tsx`'s
 * `ADMIN_BASE_PATH`).
 *
 * Every batch handed to this merge so far (the original hand-authored
 * entries and staging batches A/B/C) wrote routes WITHOUT the `/app` prefix
 * — `/chat`, `/agents/latest/:agentId`, `/toolkits/create`, etc. — which
 * `docs-shots.ts` then opens literally, landing on whatever the traefik
 * default vhost or a 404 serves at that bare path rather than the intended
 * screen. Batch D (admin) got it right by writing `/admin/app/...` routes
 * directly. So the fix belongs here, applied once to both existing and
 * incoming entries, rather than re-explained to every future writer: a
 * route that is not already `/app/...` or `/admin/...` gets `/app`
 * prepended.
 */
function normalizeRoute(route) {
  if (route === "/app" || route.startsWith("/app/")) return route;
  if (route === "/admin" || route.startsWith("/admin/")) return route;
  return `/app${route}`;
}

/** Fills in the same defaults docs-shots.ts applies at capture time, so two
 * entries that differ only by an omitted default do not read as a conflict. */
function normalizeShot(shot, source) {
  if (typeof shot !== "object" || shot === null)
    fail(`${source}: entry is not an object`);
  for (const field of REQUIRED_FIELDS) {
    if (typeof shot[field] !== "string" || shot[field] === "") {
      fail(`${source}: entry missing required string field "${field}"`);
    }
  }
  for (const key of Object.keys(shot)) {
    if (!KNOWN_FIELDS.has(key))
      fail(`${source}: entry "${shot.id}" has unknown field "${key}"`);
  }
  return {
    id: shot.id,
    route: normalizeRoute(shot.route),
    viewport: normalizeViewport(shot.viewport, source, shot.id),
    ...(shot.selector !== undefined ? { selector: shot.selector } : {}),
    ...(shot.actions !== undefined ? { actions: shot.actions } : {}),
    ...(shot.mask !== undefined ? { mask: shot.mask } : {}),
    ...(shot.persona !== undefined ? { persona: shot.persona } : {}),
    ...(shot.theme !== undefined ? { theme: shot.theme } : {}),
    ...(shot.notes !== undefined ? { notes: shot.notes } : {}),
    ...(shot.seed !== undefined ? { seed: shot.seed } : {}),
    ...(shot.placeholders !== undefined ? { placeholders: shot.placeholders } : {}),
  };
}

/** Extracts the current `shots` array out of the manifest file's source text
 * and evaluates it as a plain JS array literal (the file declares no `as
 * const`/type assertions inside individual entries, so this is valid JS). */
function readExistingShots(manifestSource) {
  const marker = "export const shots: readonly Shot[] = ";
  const start = manifestSource.indexOf(marker);
  if (start === -1) fail(`could not find "${marker}" in ${MANIFEST_PATH}`);
  // Search from AFTER the marker, not from its start: the marker text itself
  // contains "[]" (the `Shot[]` type annotation), so searching from `start`
  // finds that bracket instead of the array literal's real opening one.
  const arrayStart = manifestSource.indexOf("[", start + marker.length);
  // Balance brackets by hand rather than a greedy regex — entry values may
  // themselves contain `[` (a `mask`/`actions` array).
  let depth = 0;
  let end = -1;
  for (let i = arrayStart; i < manifestSource.length; i += 1) {
    const char = manifestSource[i];
    if (char === "[") depth += 1;
    else if (char === "]") {
      depth -= 1;
      if (depth === 0) {
        end = i + 1;
        break;
      }
    }
  }
  if (end === -1) fail(`unterminated shots array in ${MANIFEST_PATH}`);
  const arrayText = manifestSource.slice(arrayStart, end);
  const before = manifestSource.slice(0, arrayStart);
  const after = manifestSource.slice(end);
  let shots;
  try {
    // `DEFAULT_VIEWPORT` is passed in explicitly: `Function` closes over the
    // global scope only, and existing entries reference that module-local
    // const by name.
    // eslint-disable-next-line no-new-func -- trusted, repo-local source file.
    shots = new Function("DEFAULT_VIEWPORT", `return (${arrayText});`)(
      DEFAULT_VIEWPORT,
    );
  } catch (error) {
    fail(
      `could not evaluate the existing shots array in ${MANIFEST_PATH}: ` +
        `${error instanceof Error ? error.message : String(error)}`,
    );
  }
  return { shots, before, after };
}

function serializeString(value) {
  return `'${String(value).replace(/\\/g, "\\\\").replace(/'/g, "\\'")}'`;
}

function serializeValue(value) {
  if (typeof value === "string") return serializeString(value);
  if (typeof value === "number" || typeof value === "boolean")
    return String(value);
  if (Array.isArray(value)) return `[${value.map(serializeValue).join(", ")}]`;
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value).map(
      ([key, val]) =>
        `${/^[A-Za-z_$][\w$]*$/.test(key) ? key : serializeString(key)}: ${serializeValue(val)}`,
    );
    return `{ ${entries.join(", ")} }`;
  }
  fail(`cannot serialize value of type ${typeof value}`);
  return "";
}

/** Serializes one shot, collapsing a `viewport` that matches the file's
 * `DEFAULT_VIEWPORT` constant back to that identifier instead of an
 * expanded object literal — otherwise every entry evaluated out of the
 * existing array (which references the constant by name) would lose that
 * reference on the first merge, and `DEFAULT_VIEWPORT` would end up
 * unused (`tsc` TS6133). */
function serializeShot(shot) {
  const entries = Object.entries(shot).map(([key, val]) => {
    if (
      key === "viewport" &&
      JSON.stringify(val) === JSON.stringify(DEFAULT_VIEWPORT)
    ) {
      return `${key}: DEFAULT_VIEWPORT`;
    }
    return `${key}: ${serializeValue(val)}`;
  });
  return `{ ${entries.join(", ")} }`;
}

function serializeShots(shots) {
  const body = shots.map((shot) => `  ${serializeShot(shot)},`).join("\n");
  return `[\n${body}\n]`;
}

function main() {
  const dirArg = process.argv[2];
  if (!dirArg) fail("usage: node scripts/docs-shots-merge.mjs <staging-dir>");
  const stagingFiles = readStagingDir(resolve(dirArg));

  const manifestSource = readFileSync(MANIFEST_PATH, "utf8");
  const {
    shots: existingShots,
    before,
    after,
  } = readExistingShots(manifestSource);

  /** @type {Map<string, { shot: object, source: string }>} */
  const byId = new Map();
  for (const shot of existingShots) {
    byId.set(shot.id, {
      shot: normalizeShot(shot, MANIFEST_PATH),
      source: "shots.manifest.ts",
    });
  }

  for (const { name, path } of stagingFiles) {
    let parsed;
    try {
      parsed = JSON.parse(readFileSync(path, "utf8"));
    } catch (error) {
      fail(
        `${name}: invalid JSON (${error instanceof Error ? error.message : String(error)})`,
      );
      return;
    }
    if (!Array.isArray(parsed)) fail(`${name}: must be a JSON array of shots`);
    for (const raw of parsed) {
      const shot = normalizeShot(raw, name);
      const existing = byId.get(shot.id);
      if (existing === undefined) {
        byId.set(shot.id, { shot, source: name });
        continue;
      }
      const same = JSON.stringify(existing.shot) === JSON.stringify(shot);
      if (!same) {
        fail(
          `id "${shot.id}" is defined differently in ${existing.source} and ${name}. ` +
            `Fix one of them (or rename the id) and re-run.`,
        );
      }
      // Identical duplicate — keep the first, drop the rest.
    }
  }

  const merged = Array.from(byId.values(), (entry) => entry.shot);
  const rewritten = `${before}${serializeShots(merged)}${after}`;
  writeFileSync(MANIFEST_PATH, rewritten);

  try {
    execFileSync("npx", ["prettier", "--write", MANIFEST_PATH], {
      cwd: WEB_ROOT,
      stdio: "inherit",
    });
  } catch (error) {
    console.warn(
      `docs-shots-merge: wrote ${MANIFEST_PATH} but prettier formatting failed ` +
        `(${error instanceof Error ? error.message : String(error)}); the file is valid, just unformatted.`,
    );
  }

  console.log(
    `docs-shots-merge: ${merged.length} shot(s) in shots.manifest.ts ` +
      `(${stagingFiles.length} staging file(s) folded in from ${dirArg}).`,
  );
}

main();
