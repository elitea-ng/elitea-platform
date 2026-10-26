#!/usr/bin/env node
/**
 * spec §3.5 (last two rows) — bundle budget gate.
 *
 * Issue #426. The step this replaces asked whether `lighthouserc.json` existed
 * and printed "lhci bundle budget pending" when it did not. Neither that file
 * nor `.lighthouserc.json` has ever been in the tree, so the step named
 * "Bundle budget (§3.5 — 300 KB initial, 250 KB per route chunk)" was a green
 * tick for a budget that nothing measured.
 *
 * This gate measures the build output instead of asking whether a config file
 * is present. It derives its whole subject from `dist/`:
 *
 *   • the target list comes from the directories the build produced;
 *   • each target's initial set comes from that target's own HTML — the entry
 *     `<script type="module">` plus every `<link rel="modulepreload">`, which
 *     is exactly what a browser fetches before first paint;
 *   • the route-chunk set is every other JavaScript file the target emitted,
 *     `.mjs` included (Vite keeps a package's own `.mjs` extension, so a
 *     `.js`-only scan would never see a bundled module worker);
 *   • a Web Worker (`*.worker*`) runs off the main thread and is fetched only
 *     when its feature opens, so it is held to `workerChunkKB` when the target
 *     declares one, else to `routeChunkKB`.
 *
 * Every count has a floor, and the budget file must be present. A run that
 * finds no target, no HTML, no chunk or no budget FAILS. There is no
 * "nothing found, therefore OK" branch.
 *
 * Usage: node scripts/check-bundle-budget.mjs [--dist <dir>] [--budget <file>]
 * Run it after `npm run build`.
 */
import { gzipSync } from 'node:zlib';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function parseArgs(argv) {
  const args = { dist: join(APP_DIR, 'dist'), budget: join(APP_DIR, 'bundle-budget.json') };
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i];
    const value = argv[i + 1];
    if (flag === '--dist' || flag === '--budget') {
      if (!value) throw new Error(`${flag} requires an argument`);
      args[flag.slice(2)] = resolve(value);
      i++;
    } else {
      throw new Error(`unknown argument: ${flag}`);
    }
  }
  return args;
}

const fail = (message) => {
  console.error(`check-bundle-budget: FAIL — ${message}`);
  process.exit(1);
};

/** Every .js and .mjs file the target emitted, as paths relative to the target root. */
function javascriptFiles(root, prefix = '') {
  const out = [];
  for (const entry of readdirSync(root)) {
    const full = join(root, entry);
    if (statSync(full).isDirectory()) {
      out.push(...javascriptFiles(full, `${prefix}${entry}/`));
    } else if (entry.endsWith('.js') || entry.endsWith('.mjs')) {
      out.push(`${prefix}${entry}`);
    }
  }
  return out;
}

/**
 * The chunks a browser fetches before first paint: the entry module and every
 * chunk the bundler told it to preload. Read out of the built HTML, so a
 * change to the chunking strategy moves this set on its own.
 */
function initialChunks(html) {
  const names = new Set();
  // Every pattern below is case-insensitive. HTML tag names and attribute
  // names are case-insensitive. A case-sensitive read therefore misses <SCRIPT>
  // and SRC=, drops that chunk from the initial set, and makes the budget
  // report less than the browser fetches.
  const attribute = /(?:src|href)="([^"]+\.m?js)"/gi;
  const tags = html.match(/<(?:script|link)\b[^>]*>/gi) ?? [];
  for (const tag of tags) {
    if (/<link\b/i.test(tag) && !/rel="modulepreload"/i.test(tag)) continue;
    for (const match of tag.matchAll(attribute)) {
      // Skip an absolute URL to another origin or another service's route:
      // it is not a file this build emitted.
      if (/^(?:https?:)?\/\//.test(match[1])) continue;
      names.add(basename(match[1]));
    }
  }
  return names;
}

const KIB = 1024;
const gzipKiB = (path) => Math.round((gzipSync(readFileSync(path)).length / KIB) * 10) / 10;

function main() {
  const { dist, budget: budgetPath } = parseArgs(process.argv.slice(2));

  let budget;
  try {
    budget = JSON.parse(readFileSync(budgetPath, 'utf8'));
  } catch (error) {
    fail(
      `cannot read the budget file ${budgetPath}: ${error.message}. ` +
        'This gate does not run without a reviewed budget. Restore the file, or delete this gate; do not let it pass.',
    );
  }
  const declared = budget.targets ?? {};
  if (Object.keys(declared).length === 0) fail(`${budgetPath} declares no target`);

  let built;
  try {
    built = readdirSync(dist).filter((entry) => statSync(join(dist, entry)).isDirectory());
  } catch (error) {
    fail(`cannot read the build output ${dist}: ${error.message}. Run \`npm run build\` first.`);
  }
  if (built.length === 0) fail(`${dist} holds no build target`);

  // Drift in both directions. A renamed output directory, or a target the
  // build stopped producing, is a red job rather than a shorter loop.
  const missing = Object.keys(declared).filter((name) => !built.includes(name));
  const undeclared = built.filter((name) => !(name in declared));
  if (missing.length > 0) {
    fail(
      `${basename(budgetPath)} declares target(s) ${missing.join(', ')} that the build did not produce. ` +
        `The build made: ${built.join(', ')}. Correct the budget file or the build.`,
    );
  }
  if (undeclared.length > 0) {
    fail(
      `the build produced target(s) ${undeclared.join(', ')} that ${basename(budgetPath)} does not declare. ` +
        'Give each new target a budget; an unmeasured target is not a passing target.',
    );
  }

  const breaches = [];
  for (const [name, limits] of Object.entries(declared)) {
    const root = join(dist, name);
    const htmlName = limits.html ?? 'index.html';
    let html;
    try {
      html = readFileSync(join(root, htmlName), 'utf8');
    } catch (error) {
      fail(`target ${name}: cannot read ${htmlName}: ${error.message}`);
    }

    const emitted = javascriptFiles(root);
    const minChunks = limits.minJsChunks ?? 1;
    if (emitted.length < minChunks) {
      fail(
        `target ${name}: found ${emitted.length} JavaScript file(s), under the floor of ${minChunks}. ` +
          'The scan stopped matching, so a clean result here proves nothing.',
      );
    }
    if (emitted.length === 0) {
      console.log(`check-bundle-budget ${name}: no JavaScript, nothing to measure (floor ${minChunks})`);
      continue;
    }

    const initial = initialChunks(html);
    const initialFiles = emitted.filter((file) => initial.has(basename(file)));
    if (initialFiles.length === 0) {
      fail(
        `target ${name}: ${htmlName} names no emitted JavaScript chunk, yet the target emitted ` +
          `${emitted.length} of them. The HTML parse stopped matching; the initial bundle cannot be measured.`,
      );
    }

    const initialKiB = initialFiles.reduce((total, file) => total + gzipKiB(join(root, file)), 0);
    const routeFiles = emitted.filter((file) => !initial.has(basename(file)));
    console.log(
      `check-bundle-budget ${name}: initial ${initialKiB.toFixed(1)} KiB gzip over ` +
        `${initialFiles.length} chunk(s), ${routeFiles.length} route chunk(s)`,
    );

    if (limits.initialKB !== undefined && initialKiB > limits.initialKB) {
      breaches.push(
        `${name}: initial bundle is ${initialKiB.toFixed(1)} KiB gzip, over the ${limits.initialKB} KiB budget`,
      );
    }
    // A per-chunk limit is matched by file-name prefix (`elk-<hash>.js` for the
    // key `elk`) and wins over the route-chunk budget. An entry that matches
    // no emitted chunk fails: a renamed or dropped chunk would otherwise leave
    // a stale allowance behind.
    const chunkLimits = limits.chunkLimits ?? {};
    for (const key of Object.keys(chunkLimits)) {
      if (!emitted.some((file) => basename(file).startsWith(`${key}-`))) {
        fail(
          `target ${name}: chunkLimits entry "${key}" matches no emitted chunk. ` +
            'Remove the entry or correct its name; a limit that guards nothing is not a limit.',
        );
      }
    }
    for (const file of routeFiles) {
      const worker = /\.worker\b/.test(basename(file));
      const key = Object.keys(chunkLimits).find((prefix) => basename(file).startsWith(`${prefix}-`));
      const limit =
        key !== undefined
          ? chunkLimits[key].kb
          : worker
            ? (limits.workerChunkKB ?? limits.routeChunkKB)
            : limits.routeChunkKB;
      if (limit === undefined) continue;
      const size = gzipKiB(join(root, file));
      if (size > limit) {
        breaches.push(
          `${name}: ${worker ? 'worker' : 'route chunk'} ${file} is ${size.toFixed(1)} KiB gzip, over the ${limit} KiB budget`,
        );
      }
    }
  }

  if (breaches.length > 0) {
    for (const breach of breaches) console.error(`BUNDLE ${breach}`);
    console.error(`check-bundle-budget: ${breaches.length} budget breach(es) — failing (spec §3.5)`);
    process.exit(1);
  }
  console.log('check-bundle-budget: OK — every target is within its §3.5 budget');
}

main();
