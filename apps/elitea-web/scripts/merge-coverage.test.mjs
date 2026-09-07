import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, describe, expect, it } from 'vitest';

/**
 * Behavioural tests for the ONE call in this repository that enforces the
 * coverage thresholds (issue #487).
 *
 * `fileThresholdRules` was an empty array. enforceThresholds() walked every
 * file, looked for a matching rule and found none, so the script applied only
 * the four global floors while reading as a per-layer gate. Both cases below
 * pass on that version of the script and fail on nothing:
 *
 *   • "a layer below its floor" — the layer is at 0% and the global floors are
 *     still met, so the empty-rule-list version exits 0.
 *   • "a layer glob that matches no file" — the empty-rule-list version has no
 *     glob to match, so it exits 0 there too.
 *
 * The script is run as a child process, against a synthetic merged
 * coverage-final.json in a temporary directory, so no real coverage run is
 * needed.
 */

const SCRIPT = fileURLToPath(new URL('./merge-coverage.mjs', import.meta.url));

let workspace = null;

afterEach(() => {
  if (workspace) {
    rmSync(workspace, { recursive: true, force: true });
    workspace = null;
  }
});

/**
 * One synthetic file: a single statement, a single function and a two-way
 * branch, either fully covered or not covered at all.
 */
function fileCoverage(absolutePath, covered) {
  const at = (line, column) => ({ line, column });
  const loc = { start: at(1, 0), end: at(1, 20) };
  return {
    path: absolutePath,
    statementMap: { 0: loc },
    fnMap: { 0: { name: 'run', decl: loc, loc, line: 1 } },
    branchMap: { 0: { loc, type: 'branch', locations: [loc, loc], line: 1 } },
    s: { 0: covered ? 1 : 0 },
    f: { 0: covered ? 1 : 0 },
    b: { 0: covered ? [1, 1] : [0, 0] },
  };
}

/**
 * Every layer the ratchet names, so no rule matches zero files by default.
 *
 * One file per RULE, not per layer: a rule that matches nothing fails the gate
 * by design (#426), so a rule added to merge-coverage.mjs without a file here
 * fails these two cases immediately. That is the intended coupling — it is how
 * this fixture stays a real subject rather than a list somebody forgot.
 *
 * The last three are the toolkit slices, which sit INSIDE `src/features/**`
 * and `src/pages/**`. Their files therefore count toward two aggregates each,
 * which is what the overlapping-rule note in matchesPattern describes.
 */
const LAYER_FILES = [
  'src/shared/api/client.ts',
  'src/shared/config/env.ts',
  'src/shared/brand/theme.ts',
  'src/shared/lib/date.ts',
  'src/entities/agent/model.ts',
  'src/features/chat/send.ts',
  'src/processes/boot/start.ts',
  'src/widgets/nav/Nav.tsx',
  'src/pages/home/HomePage.tsx',
  'src/features/toolkits/ui/ToolBase.tsx',
  'src/pages/toolkits/Toolkits.tsx',
  'src/routes/_shell/toolkits/$tab.tsx',
  // The support-assistant, inventory and wiki slices (issue 77).
  'src/widgets/support-assistant/ui/SupportAssistantWidget.tsx',
  'src/entities/inventory/api/inventoryToolApi.ts',
  'src/features/inventory-browser/ui/InventoryBrowser.tsx',
  'src/features/inventory-chat/ui/InventoryAskPanel.tsx',
  'src/features/inventory-sources/ui/InventorySources.tsx',
  'src/features/inventory-stats/ui/InventoryStatsPanel.tsx',
  'src/pages/inventory/InventoryToolkit.tsx',
  'src/widgets/inventory/ui/InventoryWorkspace.tsx',
  'src/entities/wiki/api/wikiToolkitApi.ts',
  'src/features/wiki-browser/ui/WikiList.tsx',
  'src/features/wiki-chat/model/useWikiChat.ts',
  'src/features/wiki-editing/lib/mermaidBlocks.ts',
  'src/features/wiki-generation/model/reducer.ts',
  'src/features/wiki-settings/lib/settingsForm.ts',
  'src/pages/deepwiki/DeepWiki.tsx',
  'src/widgets/deepwiki/ui/WikiPageReader.tsx',
];

/**
 * THE RECORDED FLOORS — the ratchet issue 77 asked to be pinned in this gate's
 * own test.
 *
 * The floor list in merge-coverage.mjs is a number in a file, and a number in a
 * file can be edited downwards to make a red build green. That is exactly what
 * happened on 2026-08-05 (commit 3bcf318c lowered four global floors and
 * replaced the per-file table with a lower one), and nothing failed, because
 * the only thing checking the floors was the floors.
 *
 * This table is the second copy, and the test below fails when the script's
 * copy sits BELOW it. Raising a floor means raising it in both places, which is
 * one deliberate edit; lowering one means editing this table down, which shows
 * up in review as what it is. Adding a NEW rule needs an entry here too — a
 * rule the ratchet does not know about is a rule that can be lowered freely.
 *
 * Recorded 2026-09-07 from the re-measurement documented in merge-coverage.mjs.
 */
const RECORDED_FLOORS = {
  'src/shared/api/**': { lines: 93, statements: 92, functions: 84, branches: 90 },
  'src/shared/config/**': { lines: 93, statements: 93, functions: 81, branches: 92 },
  'src/shared/brand/**': { lines: 97, statements: 97, functions: 96, branches: 91 },
  'src/shared/lib/**': { lines: 84, statements: 83, functions: 84, branches: 77 },
  'src/entities/**': { lines: 80, statements: 80, functions: 76, branches: 81 },
  'src/features/**': { lines: 83, statements: 82, functions: 81, branches: 75 },
  'src/processes/**': { lines: 88, statements: 88, functions: 82, branches: 80 },
  'src/widgets/**': { lines: 76, statements: 74, functions: 73, branches: 69 },
  'src/pages/**': { lines: 84, statements: 82, functions: 79, branches: 75 },
  'src/features/toolkits/**': { lines: 91, statements: 90, functions: 88, branches: 81 },
  'src/pages/toolkits/**': { lines: 91, statements: 88, functions: 85, branches: 82 },
  'src/routes/_shell/toolkits/**': { lines: 97, statements: 97, functions: 97, branches: 97 },
  'src/widgets/support-assistant/**': { lines: 94, statements: 92, functions: 92, branches: 86 },
  'src/entities/inventory/**': { lines: 97, statements: 97, functions: 97, branches: 97 },
  'src/features/inventory-browser/**': { lines: 97, statements: 97, functions: 97, branches: 97 },
  'src/features/inventory-chat/**': { lines: 96, statements: 97, functions: 97, branches: 91 },
  'src/features/inventory-sources/**': { lines: 97, statements: 95, functions: 97, branches: 93 },
  'src/features/inventory-stats/**': { lines: 97, statements: 97, functions: 97, branches: 94 },
  'src/pages/inventory/**': { lines: 97, statements: 97, functions: 97, branches: 97 },
  'src/widgets/inventory/**': { lines: 90, statements: 89, functions: 86, branches: 90 },
  'src/entities/wiki/**': { lines: 92, statements: 88, functions: 89, branches: 83 },
  'src/features/wiki-browser/**': { lines: 97, statements: 97, functions: 97, branches: 81 },
  'src/features/wiki-chat/**': { lines: 97, statements: 97, functions: 97, branches: 90 },
  'src/features/wiki-editing/**': { lines: 97, statements: 97, functions: 97, branches: 97 },
  'src/features/wiki-generation/**': { lines: 97, statements: 96, functions: 97, branches: 91 },
  'src/features/wiki-settings/**': { lines: 97, statements: 97, functions: 97, branches: 91 },
  'src/pages/deepwiki/**': { lines: 86, statements: 86, functions: 74, branches: 79 },
  'src/widgets/deepwiki/**': { lines: 93, statements: 91, functions: 90, branches: 79 },
};

/**
 * The global floors, recorded for the same reason. These are the four numbers
 * commit 3bcf318c moved from 85/85/85/80 to 80/80/75/70.
 */
const RECORDED_GLOBAL_FLOORS = { lines: 80, statements: 80, functions: 75, branches: 70 };

/**
 * Read the script's floors out of its SOURCE.
 *
 * Parsed rather than imported: merge-coverage.mjs calls `main()` at module
 * scope, so importing it here would run a coverage merge in the middle of a
 * unit test.
 */
function scriptFloors() {
  const source = readFileSync(SCRIPT, 'utf8');
  const floors = {};
  const rule = /\{\s*pattern:\s*'([^']+)',\s*thresholds:\s*\{([^}]*)\}\s*\}/g;
  for (const [, pattern, body] of source.matchAll(rule)) {
    const thresholds = {};
    for (const [, key, value] of body.matchAll(/(lines|statements|functions|branches):\s*(\d+)/g)) {
      thresholds[key] = Number(value);
    }
    floors[pattern] = thresholds;
  }
  return floors;
}

function scriptGlobalFloors() {
  const source = readFileSync(SCRIPT, 'utf8');
  const block = /const globalThresholds = \{([^}]*)\}/.exec(source);
  if (block === null) return null;
  const thresholds = {};
  for (const [, key, value] of block[1].matchAll(/(lines|statements|functions|branches):\s*(\d+)/g)) {
    thresholds[key] = Number(value);
  }
  return thresholds;
}

/**
 * Build a workspace holding `coverage/coverage-final.json` and return its root.
 *
 * `keyRoot` is the prefix the coverage map's KEYS carry. It defaults to the
 * workspace, which is the normal shape. Pass a foreign prefix to reproduce the
 * artifact that a different runner wrote.
 */
function seedWorkspace(relativeFiles, keyRoot = null) {
  // realpath: macOS resolves /var/folders/... to /private/var/folders/...,
  // and the script relativises the coverage map's keys against its own cwd. A
  // symlinked prefix makes every layer glob match nothing.
  workspace = realpathSync(mkdtempSync(path.join(tmpdir(), 'merge-coverage-')));
  const root = path.join(workspace, 'app');
  mkdirSync(path.join(root, 'coverage'), { recursive: true });

  const map = {};
  for (const [relative, covered] of relativeFiles) {
    const absolute = path.posix.join(keyRoot ?? root, relative);
    map[absolute] = fileCoverage(absolute, covered);
  }
  writeFileSync(path.join(root, 'coverage', 'coverage-final.json'), JSON.stringify(map));
  return root;
}

function runValidation(root) {
  try {
    const stdout = execFileSync(process.execPath, [SCRIPT], {
      cwd: root,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { status: 0, output: stdout };
  } catch (error) {
    return { status: error.status ?? 1, output: `${error.stdout ?? ''}${error.stderr ?? ''}` };
  }
}

describe('merge-coverage.mjs threshold validation', () => {
  it('passes when every layer meets its floor', () => {
    const root = seedWorkspace(LAYER_FILES.map((file) => [file, true]));

    const result = runValidation(root);

    expect(result.status).toBe(0);
    expect(result.output).toContain('Per-layer coverage');
    expect(result.output).toContain('src/widgets/**');
  });

  it('fails on a layer below its floor while the GLOBAL floors are still met', () => {
    const root = seedWorkspace(
      LAYER_FILES.map((file) => [file, !file.startsWith('src/widgets/')]),
    );

    const result = runValidation(root);

    // Every LAYER_FILES entry but the one under src/widgets is covered, which
    // is comfortably above every global floor (80/80/75/70). Only the
    // per-layer rule can fail this run.
    expect(result.status).not.toBe(0);
    expect(result.output).toContain('layer src/widgets/**: lines 0.0% < 76%');
    expect(result.output).not.toContain('Total coverage:');
  });

  it('fails when a layer glob matches no file in the merged map (#426)', () => {
    const root = seedWorkspace([['src/app/main.tsx', true]]);

    const result = runValidation(root);

    expect(result.status).not.toBe(0);
    expect(result.output).toContain('layer src/widgets/**: matches no file in the merged coverage map');
    expect(result.output).toContain('layer src/features/**: matches no file in the merged coverage map');
  });

  it('matches the layers when the artifact was written under another root', () => {
    // coverage-merge and coverage-validation run on different runners. The
    // downloaded coverage-final.json carries the WRITER's absolute paths, so a
    // plain path.relative() gives `../../…` and every layer glob would match
    // nothing — nine "matches no file" failures over healthy coverage.
    const root = seedWorkspace(
      LAYER_FILES.map((file) => [file, true]),
      '/home/runner/work/elitea-platform/elitea-platform/apps/elitea-web',
    );

    const result = runValidation(root);

    expect(result.status).toBe(0);
    // Four files sit under `src/widgets/**` in LAYER_FILES now: the nav widget
    // plus the three slices issue 77 gave their own floors.
    expect(result.output).toContain('src/widgets/**: 4 file(s)');
  });

  it('still fails when the GLOBAL floors are missed', () => {
    // One covered file of the LAYER_FILES set: 8.3% total at twelve files.
    // The number moves whenever a rule (and so a file) is added, which is why
    // it is derived below rather than written out a second time. All zeroes
    // would trip assertInstrumentationNotBroken() first, a different gate.
    const root = seedWorkspace(
      LAYER_FILES.map((file) => [file, file === 'src/shared/api/client.ts']),
    );

    const result = runValidation(root);

    const total = ((1 / LAYER_FILES.length) * 100).toFixed(1);
    expect(result.status).not.toBe(0);
    expect(result.output).toContain(`Total coverage: lines ${total}% < 80%`);
  });
});

/**
 * A FLOOR MAY ONLY RISE (issue 77).
 *
 * The three cases below are the whole rule: no floor may drop, no rule may lose
 * its recorded copy, and no rule may arrive without one.
 */
describe('the coverage floors are a ratchet', () => {
  it('holds every recorded per-layer floor', () => {
    const actual = scriptFloors();
    const dropped = [];

    for (const [pattern, recorded] of Object.entries(RECORDED_FLOORS)) {
      const current = actual[pattern];
      if (current === undefined) continue; // reported by the case below
      for (const key of ['lines', 'statements', 'functions', 'branches']) {
        if (current[key] < recorded[key]) {
          dropped.push(`${pattern} ${key}: ${current[key]} < recorded ${recorded[key]}`);
        }
      }
    }

    expect(
      dropped,
      'A floor was lowered. Raising coverage is the fix; if a floor genuinely has'
        + ' to come down, change RECORDED_FLOORS in the same commit and say why.',
    ).toEqual([]);
  });

  it('keeps a recorded floor for every rule the script enforces', () => {
    // A rule with no recorded copy is a rule the ratchet cannot hold, which is
    // how a new slice quietly acquires a floor that may be edited downwards.
    const unrecorded = Object.keys(scriptFloors()).filter(
      (pattern) => !(pattern in RECORDED_FLOORS),
    );

    expect(unrecorded, 'Add these patterns to RECORDED_FLOORS.').toEqual([]);
  });

  it('keeps a rule for every recorded floor', () => {
    // The other direction: DELETING a rule is the cheapest way to remove a
    // floor, and it leaves no number to compare against.
    const actual = scriptFloors();
    const removed = Object.keys(RECORDED_FLOORS).filter((pattern) => !(pattern in actual));

    expect(
      removed,
      'A rule was deleted. Deleting a rule removes its floor entirely — the'
        + ' strongest form of lowering it.',
    ).toEqual([]);
  });

  it('holds the four global floors', () => {
    const actual = scriptGlobalFloors();
    expect(actual).not.toBeNull();

    const dropped = Object.entries(RECORDED_GLOBAL_FLOORS)
      .filter(([key, recorded]) => actual[key] < recorded)
      .map(([key, recorded]) => `${key}: ${actual[key]} < recorded ${recorded}`);

    expect(dropped).toEqual([]);
  });
});
