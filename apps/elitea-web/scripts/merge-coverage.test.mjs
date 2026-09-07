import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
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
];

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
    expect(result.output).toContain('layer src/widgets/**: lines 0.0% < 47%');
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
    expect(result.output).toContain('src/widgets/**: 1 file(s)');
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
