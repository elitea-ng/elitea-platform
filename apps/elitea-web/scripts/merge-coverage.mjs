#!/usr/bin/env node
import fs from 'fs/promises';
import path from 'path';
import coveragePkg from 'istanbul-lib-coverage';
import { createContext } from 'istanbul-lib-report';
import reports from 'istanbul-reports';

const { createCoverageMap, createCoverageSummary } = coveragePkg;

const root = process.cwd();
const shardRoot = path.join(root, 'coverage-shards');
const altShardRoot = path.join(root, '..', 'coverage-shards');
const outputDir = path.join(root, 'coverage');

// CLI flags — coverage-merge CI job uses --no-validate to skip threshold checks.
const args = process.argv.slice(2);
const skipValidation = args.includes('--no-validate');

const globalThresholds = {
  lines: 80,
  statements: 80,
  functions: 75,
  branches: 70,
};
// PER-LAYER FLOORS — a ratchet, not an aspiration (issue #487).
//
// This list was `[]`. enforceThresholds() still walked every file looking for
// a matching rule and found none, so matchesPattern() and the per-file half of
// checkThreshold() ran for no file at all. The script READ as a per-layer gate
// and applied only the four global floors. An empty list cannot report a
// failure, which is the same class as #421.
//
// Two things changed with the numbers below.
//
// 1. The rules are now PER LAYER, not per file. Commit 3bcf318c deleted a
//    per-FILE table (every single file under `src/pages/**` at 80% lines), and
//    a per-file floor fails on the first thin module somebody adds. A layer
//    aggregate holds the layer to a number the layer as a whole meets.
//
// 2. Each floor comes from a MEASUREMENT, not from a target. Every layer was
//    measured on 2026-08-28 on this branch with:
//
//      npx vitest run --config vitest.config.ts --project node \
//        --coverage.enabled=true --coverage.all=true \
//        --coverage.include='src/<layer>/**/*.{ts,tsx}' \
//        --coverage.reporter=json-summary src/<layer>
//
//    That command runs ONLY the layer's own tests, so each number is a LOWER
//    BOUND on what the full sharded suite produces: more tests add hits and
//    never remove them, and coverage.all=true fixes the denominator. The floor
//    is the measured value rounded down, minus two points of slack. So no
//    floor here can go red on the day it lands, and every one of them came
//    from a real run rather than from a wish.
//
//                        measured (own tests only)      floor
//    layer               lines  stmts  fns    branch    l/s/f/b
//    shared/api          95.83  94.73  86.85  92.55     93/92/84/90
//    shared/config       95.52  95.58  83.33  94.59     93/93/81/92
//    shared/brand        99.75  99.56  98.75  93.22     97/97/96/91
//    shared/lib          86.76  85.60  86.70  79.31     84/83/84/77
//    entities            79.85  79.80  76.32  81.10     77/77/74/79
//    features            84.56  83.28  81.91  75.84     82/81/79/73
//    processes           88.02  86.24  77.77  78.94     86/84/75/76
//    widgets             49.27  48.74  49.26  44.87     47/46/47/42
//    pages               81.13  79.33  76.91  72.21     79/77/74/70
//
// RAISING THE RATCHET. `widgets` reads low because most of what exercises a
// widget is a page test, and the measurement above ran neither. The
// coverage-validation job prints each layer's REAL aggregate from the merged
// shards on every run. Take those numbers and raise these floors — that is
// what a ratchet is for. Lower one only with the measurement that justifies
// it, in the pull request that lowers it.
//
// The list must never be empty again: enforceLayerThresholds() fails on an
// empty list, and fails on any rule whose glob matches no file in the merged
// coverage map (#426 — a check with no subject must fail, not pass).
//
// THE THREE TOOLKIT SLICES, added 2026-09-06 at 85/85/85/80.
//
// They are the first rules here that are NARROWER than a layer, and they are
// narrower on purpose. `src/features/**` cannot express "the toolkit surface
// stays covered": that layer aggregates 1,113 files, and the 157 under
// `features/toolkits` could fall twenty points without moving it. A percentage
// is also not by itself the thing being asked for — "every toolkit type is
// covered" is a per-TYPE property, and the tests that state it are the
// table-driven ones that enumerate the served catalogue
// (`features/toolkits/__tests__/servedCatalogue.perType.test.tsx` in this app,
// and its Go and worker siblings). These floors are the second half: they stop
// the percentage sliding back once those tests exist.
//
// Measured on this branch with the full non-sharded `node` project, before and
// after the per-type suites landed:
//
//    slice                       lines  stmts  fns    branch
//    features/toolkits  before    95.50  93.63  92.10  85.38
//                       after     95.50  93.70  92.25  85.56
//    pages/toolkits     before    89.97  84.52  81.34  71.66
//                       after     93.04  89.52  86.57  83.39
//    routes/_shell/toolkits before 66.67 66.67  25.00  100.00
//                       after    100.00 100.00 100.00 100.00
//
// `routes/_shell/toolkits` read 25% FUNCTIONS before: the three route
// components that mount the toolkit pages were executed by nothing at all.
// `allRoutesSmoke.test.tsx` mounts the whole generated tree and never reached
// them, which is the failure mode this per-slice floor now catches — a whole
// directory at zero, inside a layer that has no rule of its own.
//
// The branch floor is 80 rather than 85 for the same reason every branch floor
// above sits under its siblings: a branch counted by v8 includes every optional
// chain and default parameter, and holding those to the statement number costs
// tests that assert nothing. 80 is under the measured 83.39/85.56/100 by a
// margin, and above the 71.66 `pages/toolkits` sat at before this change.
const layerThresholdRules = [
  { pattern: 'src/shared/api/**', thresholds: { lines: 93, statements: 92, functions: 84, branches: 90 } },
  { pattern: 'src/shared/config/**', thresholds: { lines: 93, statements: 93, functions: 81, branches: 92 } },
  { pattern: 'src/shared/brand/**', thresholds: { lines: 97, statements: 97, functions: 96, branches: 91 } },
  { pattern: 'src/shared/lib/**', thresholds: { lines: 84, statements: 83, functions: 84, branches: 77 } },
  { pattern: 'src/entities/**', thresholds: { lines: 77, statements: 77, functions: 74, branches: 79 } },
  { pattern: 'src/features/**', thresholds: { lines: 82, statements: 81, functions: 79, branches: 73 } },
  { pattern: 'src/processes/**', thresholds: { lines: 86, statements: 84, functions: 75, branches: 76 } },
  { pattern: 'src/widgets/**', thresholds: { lines: 47, statements: 46, functions: 47, branches: 42 } },
  { pattern: 'src/pages/**', thresholds: { lines: 79, statements: 77, functions: 74, branches: 70 } },
  // The toolkit surface, per slice. See the note above the list.
  { pattern: 'src/features/toolkits/**', thresholds: { lines: 85, statements: 85, functions: 85, branches: 80 } },
  { pattern: 'src/pages/toolkits/**', thresholds: { lines: 85, statements: 85, functions: 85, branches: 80 } },
  { pattern: 'src/routes/_shell/toolkits/**', thresholds: { lines: 85, statements: 85, functions: 85, branches: 80 } },
];

async function main() {
  const shardFiles = await findCoverageFiles();
  if (shardFiles.length === 0) {
    // NEITHER mode may report success with nothing to measure.
    //
    // --no-validate is the coverage-merge job's mode: it merges *fresh* shard
    // artifacts and must never go green with nothing merged (the bug that let
    // this job report success while uploading an empty coverage/ directory —
    // see issue #67).
    //
    // The default mode is the coverage-validation job, the ONE call in this
    // repository that enforces the thresholds. It used to `return` here — exit
    // 0 — so the validating call passed whenever it had no numbers to validate
    // (issue #421, item 5). findCoverageFiles()'s outputDir fallback is what
    // makes that job's normal shape reachable, so arriving here means even the
    // downloaded artifact was absent.
    console.error(
      skipValidation
        ? 'No coverage shard artifacts found; refusing to report success with nothing merged.'
        : 'No coverage data found; refusing to report the thresholds as met with nothing to measure.',
    );
    process.exit(1);
  }

  const coverageMap = createCoverageMap({});
  for (const shardFile of shardFiles) {
    const raw = JSON.parse(await fs.readFile(shardFile, 'utf8'));
    coverageMap.merge(raw);
  }

  assertSomethingWasMeasured(coverageMap, shardFiles);

  printUncoveredInfo(coverageMap);

  await fs.rm(outputDir, { recursive: true, force: true });
  await fs.mkdir(outputDir, { recursive: true });

  const context = createContext({ dir: outputDir, coverageMap });

  const reportsToRender = [
    // istanbul-reports' own `json` reporter writes a real, complete
    // coverage-final.json (path/statementMap/fnMap/branchMap/s/f/b per
    // file, via JSON.stringify on each FileCoverage instance) — the file
    // downstream gate checks (gate-mutator-coverage etc.) and the
    // coverage-validation job's own re-run of this script expect. A prior
    // version of this script hand-wrote an abbreviated {path,s,b,f} object
    // here instead, which istanbul-lib-coverage's own CoverageMap.merge()
    // rejects as invalid ("missing keys") — exactly what coverage-validation
    // hit when it re-loaded coverage-merge's uploaded artifact.
    { type: 'json' },
    { type: 'json-summary' },
    { type: 'lcovonly', options: { file: 'lcov.info' } },
    { type: 'html' },
    { type: 'text-summary' },
  ];

  for (const reportDef of reportsToRender) {
    const report = reports.create(reportDef.type, reportDef.options || {});
    console.log(`  Writing ${reportDef.type} report to ${reportDef.options?.file ?? 'context.dir'}...`);
    await report.execute(context);
  }
  // Verify report files were actually written
  const files = await fs.readdir(outputDir);
  console.log(`  Report files in ${outputDir}: ${files.join(', ')}`);

  assertInstrumentationNotBroken(coverageMap);
  enforceThresholds(coverageMap, skipValidation);
  console.log('Merged coverage reports generated at', outputDir);
}

function printUncoveredLines(uncoveredLines) {
  if (uncoveredLines.length === 0) return;
  console.log('Uncovered lines per file:');
  for (const entry of uncoveredLines) {
    console.log(`  ${entry.file}: ${entry.uncoveredLines.join(', ')} (${entry.missedLines}/${entry.totalLines} lines uncovered)`);
  }
}

function printUncoveredFunctions(uncoveredFunctions) {
  if (uncoveredFunctions.length === 0) return;
  console.log('Uncovered functions per file:');
  for (const entry of uncoveredFunctions) {
    const details = entry.funcs.map((func) => `${func.name} (line ${func.line})`).join(', ');
    console.log(`  ${entry.file}: ${details}`);
  }
}

function printUncoveredInfo(coverageMap) {
  const uncoveredLines = [];
  const uncoveredFunctions = [];

  for (const file of coverageMap.files()) {
    const fileCoverage = coverageMap.fileCoverageFor(file);
    if (!fileCoverage) continue;

    const relativeFile = path.relative(root, file).replace(/\\/g, '/');
    const lines = fileCoverage.getUncoveredLines();
    const lineCoverage = fileCoverage.getLineCoverage();
    const totalLines = Object.keys(lineCoverage || {}).length;
    if (lines && lines.length > 0) {
      uncoveredLines.push({
        file: relativeFile,
        uncoveredLines: lines,
        totalLines,
        missedLines: lines.length,
      });
    }

    const funcs = getUncoveredFunctions(fileCoverage);
    if (funcs.length > 0) {
      uncoveredFunctions.push({ file: relativeFile, funcs });
    }
  }

  if (uncoveredLines.length === 0 && uncoveredFunctions.length === 0) {
    console.log('No uncovered lines or functions detected in merged coverage.');
    return;
  }

  printUncoveredLines(uncoveredLines);
  printUncoveredFunctions(uncoveredFunctions);
}

function getUncoveredFunctions(fileCoverage) {
  const uncovered = [];
  const fnMap = fileCoverage.fnMap || {};
  const hits = fileCoverage.f || {};

  for (const key of Object.keys(fnMap)) {
    const fnMeta = fnMap[key];
    const hitCount = Number(hits[key] || 0);
    if (hitCount === 0) {
      uncovered.push({
        name: fnMeta.name || '<anonymous>',
        line: fnMeta.decl?.start?.line ?? fnMeta.line ?? 'unknown',
      });
    }
  }

  return uncovered;
}

async function findCoverageFiles() {
  const files = [];
  const roots = [root, shardRoot, altShardRoot];

  const scanDirs = [root, path.join(root, '..')];
  for (const scanDir of scanDirs) {
    try {
      const entries = await fs.readdir(scanDir, { withFileTypes: true });
      for (const entry of entries) {
        if (entry.isDirectory() && entry.name.startsWith('coverage-shard-')) {
          roots.push(path.join(scanDir, entry.name));
        }
      }
    } catch {
      // ignore missing repo root or permission issues
    }
  }

  // Directories never worth descending into while hunting for shard files:
  // vendored/build output can ship its own stray coverage-final.json (e.g.
  // node_modules/tsconfig-paths-webpack-plugin ships one from its own test
  // suite) that would otherwise silently get merged in as if it were one of
  // our shards, polluting — or, worse, entirely displacing — the real data.
  const SKIP_DIR_NAMES = new Set(['node_modules', '.git', 'dist', 'storybook-static']);

  async function walk(dir) {
    let entries;
    try {
      entries = await fs.readdir(dir, { withFileTypes: true });
    } catch {
      return;
    }

    for (const entry of entries) {
      const entryPath = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (SKIP_DIR_NAMES.has(entry.name)) continue;
        // Never descend into our own output directory while looking for
        // *fresh* shards: on a rerun it may still hold a coverage-final.json
        // from a PRIOR merge, and folding that stale aggregate in alongside
        // real new shards would double-count hits for any file both cover.
        // (The coverage-validation job's own re-run — where outputDir holds
        // exactly one file, the artifact downloaded from coverage-merge, and
        // no other shard exists anywhere — is handled as an explicit
        // fallback below, not through this walk.)
        if (entryPath === outputDir) continue;
        await walk(entryPath);
      } else if (entry.isFile() && entry.name === 'coverage-final.json') {
        files.push(entryPath);
      }
    }
  }

  for (const scanRoot of roots) {
    await walk(scanRoot);
  }

  if (files.length === 0) {
    // Fallback: no fresh per-shard files anywhere, but a previously-merged
    // coverage-final.json already sits in our own output directory (this is
    // exactly the coverage-validation job's shape: it downloads
    // coverage-merge's "elitea-web-coverage" artifact straight into
    // outputDir, then re-runs this script to enforce thresholds against it).
    // Load it directly rather than reporting "nothing found" — this is a
    // validation-only rerun of already-merged data, not a fresh merge.
    const priorMerge = path.join(outputDir, 'coverage-final.json');
    if (
      await fs
        .access(priorMerge)
        .then(() => true)
        .catch(() => false)
    ) {
      console.log('No fresh shard files found; falling back to the previously-merged', priorMerge);
      files.push(priorMerge);
    } else {
      console.log('No coverage shard artifacts found in:', roots.join(', '));
    }
  }

  return files;
}

// The floor on the CONTENTS, not on the file count (issue #483, item 3).
//
// The shard-count check above proves a file arrived. It does not prove the
// file holds numbers. A `coverage-final.json` of `{}` satisfies it, and every
// gate downstream then reads zeros as a pass:
//
//   • assertInstrumentationNotBroken() is gated on `totalStatements > 0`, so a
//     total of 0 skips it;
//   • istanbul's own `percent()` returns 100 when the total is 0, so every
//     threshold in checkThreshold() is met by an empty summary.
//
// So the one call in this repository that enforces the coverage thresholds
// reported them as met over no numbers at all. The counts are printed here,
// because a reader must be able to see WHAT was measured, not only that
// something was.
function assertSomethingWasMeasured(coverageMap, shardFiles) {
  const files = coverageMap.files();
  const summary = getSummary(coverageMap.getCoverageSummary());
  const totalStatements = summary.statements?.total ?? 0;

  console.log(
    `Merged ${shardFiles.length} shard file(s): ${files.length} source file(s), ${totalStatements} statement(s).`,
  );

  if (files.length === 0 || totalStatements === 0) {
    console.error(
      `MEASURED NOTHING: the merged coverage map holds ${files.length} file(s) and`
        + ` ${totalStatements} statement(s), from ${shardFiles.length} shard file(s):`
        + ` ${shardFiles.join(', ')}.`,
    );
    console.error(
      'An empty coverage map meets every threshold, because a percentage over a'
        + ' total of zero is 100. Refusing to report success with nothing measured.',
    );
    process.exit(1);
  }
}

function assertInstrumentationNotBroken(coverageMap) {
  const summary = coverageMap.getCoverageSummary();
  const data = typeof summary.toJSON === 'function' ? summary.toJSON() : summary;
  const totalStatements = data.statements?.total ?? 0;
  const coveredStatements = data.statements?.covered ?? 0;

  if (totalStatements > 0 && coveredStatements === 0) {
    console.error(
      `INSTRUMENTATION BROKEN: ${totalStatements} statements counted but 0 covered.`
      + ' V8 coverage collection is not working — this is a provider/config bug, not a missing-tests problem.'
      + ' Check vitest.config.ts coverage.provider and any customProviderModule.',
    );
    process.exit(1);
  }
}

function enforceThresholds(coverageMap, skipValidation) {
  if (skipValidation) {
    console.log('Coverage threshold validation skipped (--no-validate).');
    return;
  }
  const summary = getSummary(coverageMap.getCoverageSummary());
  const failures = [];

  checkThreshold('Total coverage', summary, globalThresholds, failures);
  enforceLayerThresholds(coverageMap, failures);

  if (failures.length > 0) {
    console.error('Coverage threshold failures:');
    for (const failure of failures) {
      console.error(failure);
    }
    throw new Error('Coverage threshold enforcement failed');
  }
}

function getSummary(summary) {
  if (typeof summary.toJSON === 'function') {
    return summary.toJSON();
  }
  if (summary && typeof summary.data !== 'undefined') {
    return summary.data;
  }
  return summary;
}

/**
 * Hold every layer in layerThresholdRules to its own aggregate floor.
 *
 * Two guards come before the numbers, because both were the actual defect
 * rather than a hypothetical one:
 *   • an EMPTY rule list is a failure. The list used to be empty, and the walk
 *     over it reported success on every run (issue #487).
 *   • a rule matching NO file is a failure. A layer that is renamed or moved
 *     otherwise takes its floor with it silently, and the glob keeps reading
 *     as a live gate (issue #426, and the same shape as the stale coverage
 *     exclusions in #309).
 */
function enforceLayerThresholds(coverageMap, failures) {
  if (layerThresholdRules.length === 0) {
    failures.push(
      'per-layer floors: the rule list is EMPTY, so this gate cannot report a failure.'
        + ' Restore the measured floors, or delete the machinery — do not keep both.',
    );
    return;
  }

  const relatives = coverageMap.files().map((file) => ({ file, relative: layerPath(file) }));

  console.log('Per-layer coverage (aggregate over the layer, floor in brackets):');
  for (const rule of layerThresholdRules) {
    const matched = relatives.filter((entry) => matchesPattern(entry.relative, rule.pattern));
    if (matched.length === 0) {
      failures.push(
        `layer ${rule.pattern}: matches no file in the merged coverage map, so its floor gates nothing.`
          + ' Point the glob at the layer\'s new location, or delete the rule.',
      );
      continue;
    }

    const layerSummary = getSummary(summariseFiles(coverageMap, matched.map((entry) => entry.file)));
    const shown = ['lines', 'statements', 'functions', 'branches']
      .map((key) => `${key} ${layerSummary[key].pct.toFixed(2)}% [${rule.thresholds[key]}%]`)
      .join(', ');
    console.log(`  ${rule.pattern}: ${matched.length} file(s) — ${shown}`);

    checkThreshold(`layer ${rule.pattern}`, layerSummary, rule.thresholds, failures);
  }
}

/**
 * The path a layer glob is matched against.
 *
 * Normally `path.relative(cwd, key)` — the coverage map's keys are absolute
 * paths written by the shard runner, and the coverage-validation job runs from
 * apps/elitea-web on a workspace with the same layout.
 *
 * A coverage artifact produced under a DIFFERENT absolute root relativises to
 * `../../…`, which matches no layer glob and would turn the whole gate red for
 * a reason that is not coverage. Fall back to the `src/…` tail in that case,
 * and say so, so the log states which form was used.
 */
function layerPath(file) {
  const relative = path.relative(root, file).replace(/\\/g, '/');
  if (!relative.startsWith('../')) {
    return relative;
  }
  const normalised = file.replace(/\\/g, '/');
  const index = normalised.lastIndexOf('/src/');
  if (index === -1) {
    return relative;
  }
  return normalised.slice(index + 1);
}

/** Aggregate the given files into one coverage summary. */
function summariseFiles(coverageMap, files) {
  const summary = createCoverageSummary();
  for (const file of files) {
    summary.merge(coverageMap.fileCoverageFor(file).toSummary());
  }
  return summary;
}

function matchesPattern(filePath, pattern) {
  if (pattern.endsWith('/**')) {
    // `slice(0, -2)` keeps the separator, so `src/shared/api/**` matches
    // `src/shared/api/client.ts` and NOT a sibling named `src/shared/apiX.ts`.
    return filePath.startsWith(pattern.slice(0, -2));
  }
  return filePath === pattern;
}

function checkThreshold(name, actual, expected, failures) {
  for (const key of ['lines', 'statements', 'functions', 'branches']) {
    const actualPct = actual[key]?.pct ?? null;
    const requiredPct = expected[key];
    if (requiredPct != null && actualPct != null && actualPct < requiredPct) {
      failures.push(`${name}: ${key} ${actualPct.toFixed(1)}% < ${requiredPct}%`);
    }
  }
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
