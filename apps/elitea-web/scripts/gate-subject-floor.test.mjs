import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';

/**
 * The RED/GREEN proof for issue #528: a gate whose subject set is empty must
 * FAIL, not pass.
 *
 * Every case here spawns the REAL gate script over a real, emptied subject
 * set. Nothing is stubbed. The scripts resolve their inputs relative to
 * `<script>/..`, so each case builds a miniature app in a temporary directory,
 * copies the script and the modules it imports into it, and symlinks the app's
 * `node_modules` so the copies resolve their own dependencies.
 *
 * Two of the eight gates are proved elsewhere, and both for the same reason —
 * the subject set is not something a fixture can supply:
 *   - `check-generated-client.mjs` compares the checkout against a live `npx
 *     orval` run. Its empty case is pinned in
 *     scripts/lib/generated-client-tree.test.mjs, where an empty tree pair is
 *     shown to report `ok: true` over zero subjects.
 *   - `check-endpoint-manifest.mjs` takes `--manifest`, so its empty case
 *     lives with its other CLI cases in
 *     scripts/check-endpoint-manifest.test.mjs.
 *
 * check-testid-namespace.mjs is proved in its own file for the same reason as
 * check-endpoint-manifest.mjs: it takes `--root`, so its empty case sits with
 * its other CLI cases in scripts/check-testid-namespace.test.mjs.
 */

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_ROOT = join(SCRIPT_DIR, '..');

let dirs = [];

afterEach(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
  dirs = [];
});

/**
 * Build a miniature app around one gate script.
 *
 * @param {object} spec
 * @param {string} spec.script the gate script's file name.
 * @param {string[]} [spec.libs] modules under scripts/lib the script imports.
 * @param {Record<string, string>} [spec.files] app-relative path -> content.
 * @param {string[]} [spec.dirs] app-relative directories to create empty.
 * @returns {{root: string, script: string}}
 */
function makeApp({ script, libs = [], files = {}, dirs: emptyDirs = [] }) {
  const root = mkdtempSync(join(tmpdir(), 'gate-floor-'));
  dirs.push(root);

  // The copies import from `node_modules`, and node resolves that by walking
  // up from the file. One symlink at the app root is the whole recipe.
  symlinkSync(join(APP_ROOT, 'node_modules'), join(root, 'node_modules'), 'dir');

  mkdirSync(join(root, 'scripts', 'lib'), { recursive: true });
  copyFileSync(join(SCRIPT_DIR, script), join(root, 'scripts', script));
  for (const lib of ['gate-floor.mjs', ...libs]) {
    copyFileSync(join(SCRIPT_DIR, 'lib', lib), join(root, 'scripts', 'lib', lib));
  }

  for (const dir of emptyDirs) mkdirSync(join(root, dir), { recursive: true });
  for (const [file, content] of Object.entries(files)) {
    mkdirSync(dirname(join(root, file)), { recursive: true });
    writeFileSync(join(root, file), content);
  }

  return { root, script: join(root, 'scripts', script) };
}

function run(app, args = []) {
  return spawnSync(process.execPath, [app.script, ...args], { cwd: app.root, encoding: 'utf8' });
}

// ── 1. check-visual-coverage.mjs ────────────────────────────────────────────
describe('check-visual-coverage: an empty screenshot index', () => {
  const shot = (route, wiringStatus) => ({ route, wiringStatus });

  /** An index that clears every floor, so each case empties exactly one set. */
  function healthyIndex() {
    const shots = [];
    for (let i = 0; i < 45; i++) shots.push(shot(`/wired-${i}`, 'wired'));
    for (let i = 0; i < 20; i++) shots.push(shot(`/pending-${i}`, 'ready'));
    return JSON.stringify({ shots });
  }

  function coveringSpec() {
    const lines = [];
    for (let i = 0; i < 45; i++) lines.push(`// @covers /wired-${i}`);
    return `${lines.join('\n')}\n`;
  }

  function baselines(count) {
    const files = {};
    for (let i = 0; i < count; i++) files[`e2e/snapshots/visual/shot-${i}.png`] = 'x';
    return files;
  }

  it('RED — `"shots": []` exits non-zero instead of reporting every wired route covered', () => {
    const app = makeApp({
      script: 'check-visual-coverage.mjs',
      files: {
        'parity/screenshot-index.json': JSON.stringify({ shots: [] }),
        'e2e/visual/routes.visual.spec.ts': coveringSpec(),
        ...baselines(30),
      },
    });

    const result = run(app);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 indexed shots in parity/screenshot-index.json');
    expect(result.stdout).not.toContain('OK — every wired route has a spec');
  });

  it("RED — a renamed `wiringStatus` value empties the enforced subset", () => {
    const shots = [];
    for (let i = 0; i < 65; i++) shots.push(shot(`/route-${i}`, 'connected'));
    const app = makeApp({
      script: 'check-visual-coverage.mjs',
      files: {
        'parity/screenshot-index.json': JSON.stringify({ shots }),
        'e2e/visual/routes.visual.spec.ts': coveringSpec(),
        ...baselines(30),
      },
    });

    const result = run(app);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain("0 indexed shots with wiringStatus 'wired'");
  });

  it('RED — deleting every baseline no longer meets the size budget', () => {
    const app = makeApp({
      script: 'check-visual-coverage.mjs',
      files: {
        'parity/screenshot-index.json': healthyIndex(),
        'e2e/visual/routes.visual.spec.ts': coveringSpec(),
      },
    });

    const result = run(app);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 committed baseline files under e2e/snapshots');
  });

  it('GREEN — a full index with baselines still passes, and states all three counts', () => {
    const app = makeApp({
      script: 'check-visual-coverage.mjs',
      files: {
        'parity/screenshot-index.json': healthyIndex(),
        'e2e/visual/routes.visual.spec.ts': coveringSpec(),
        ...baselines(30),
      },
    });

    const result = run(app);

    expect(result.status).toBe(0);
    expect(result.stdout).toContain('measured 65 indexed shots');
    expect(result.stdout).toContain("measured 45 indexed shots with wiringStatus 'wired'");
    expect(result.stdout).toContain('measured 30 committed baseline files');
    expect(result.stdout).toContain('OK — every wired route has a spec');
  });
});

// ── 2. i18n-backfill.mjs --check ────────────────────────────────────────────
describe('i18n-backfill --check: an extraction that matches nothing', () => {
  // `i18n-backfill-core.mjs` reads its directory walker from `budgets-core`.
  const LIBS = ['i18n-backfill-core.mjs', 'budgets-core.mjs'];

  it('RED — a src tree with no source file exits non-zero instead of reporting OK', () => {
    const app = makeApp({
      script: 'i18n-backfill.mjs',
      libs: LIBS,
      dirs: ['src'],
      files: { 'src/shared/i18n/en.json': JSON.stringify({}) },
    });

    const result = run(app, ['--check']);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 source files scanned under src/');
    expect(result.stdout).not.toContain('i18n-backfill --check: OK');
  });

  it('RED — a renamed `@/shared/i18n` module empties the call-site set', () => {
    const files = { 'src/shared/i18n/en.json': JSON.stringify({}) };
    // Enough files to clear the file-count floor, every one of them importing
    // `t` from somewhere else — which is exactly what a module rename does.
    for (let i = 0; i < 1600; i++) {
      files[`src/generated/file-${i}.ts`] = "import { t } from '@/shared/i18n-renamed';\nexport const a = t('k', 'v');\n";
    }
    const app = makeApp({ script: 'i18n-backfill.mjs', libs: LIBS, files });

    const result = run(app, ['--check']);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 t() call sites bound to @/shared/i18n');
  });
});

// ── 3. check-contract-coverage.mjs ──────────────────────────────────────────
describe('check-contract-coverage: an emptied spec', () => {
  /**
   * This gate takes `--spec` and `--lock`, so it needs no miniature app. The
   * lock is emptied on purpose: the burn-down ratchet is what caught this case
   * before, and only by accident of the lock's contents.
   */
  function fixtures() {
    const dir = mkdtempSync(join(tmpdir(), 'gate-floor-spec-'));
    dirs.push(dir);
    const spec = join(dir, 'empty-spec.yaml');
    const lock = join(dir, 'empty-lock.json');
    writeFileSync(spec, 'openapi: 3.0.3\ninfo:\n  title: emptied\n  version: "1"\npaths: {}\n');
    writeFileSync(lock, JSON.stringify({ version: 1, generated: [] }));
    return { spec, lock };
  }

  it('RED — `paths: {}` exits non-zero instead of classifying everything handwritten', () => {
    const { spec, lock } = fixtures();
    const result = spawnSync(
      process.execPath,
      [join(SCRIPT_DIR, 'check-contract-coverage.mjs'), '--spec', spec, '--lock', lock],
      { cwd: APP_ROOT, encoding: 'utf8' },
    );

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 operations with an operationId');
    expect(result.stdout).not.toContain('contract-coverage: OK');
  });
});

// ── 4. build-route-wiring-map.mjs --check ───────────────────────────────────
describe('build-route-wiring-map --check: a walk that finds no route', () => {
  it('RED — an empty src/routes exits non-zero, even against a matching artifact', () => {
    const app = makeApp({
      script: 'build-route-wiring-map.mjs',
      dirs: ['src/routes'],
    });

    // Write the artifact the zero-route run itself produces. Without a floor,
    // `--check` compares that to itself and agrees forever.
    const written = run(app);
    expect(written.status).not.toBe(0);
    expect(written.stderr).toContain('0 route files walked under src/routes');

    const checked = run(app, ['--check']);
    expect(checked.status).not.toBe(0);
    expect(checked.stderr).toContain('0 route files walked under src/routes');
    expect(checked.stdout).not.toContain('up to date');
  });
});

// ── 5. check-dead-code.mjs ──────────────────────────────────────────────────
describe('check-dead-code: knip globs that match nothing', () => {
  it('RED — an empty `project` glob set exits non-zero before knip runs', () => {
    const app = makeApp({
      script: 'check-dead-code.mjs',
      libs: ['jsonc.mjs'],
      files: {
        'knip.json': JSON.stringify({ entry: ['src/main.tsx'], project: ['renamed-src/**/*.ts'] }),
      },
    });

    const result = run(app);

    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('0 files matched by knip.json `project`');
  });

  it('RED — a knip.json that cannot be read is a failure, not an empty analysis', () => {
    const app = makeApp({
      script: 'check-dead-code.mjs',
      libs: ['jsonc.mjs'],
      files: { 'knip.json': '{ this is not json' },
    });

    const result = run(app);

    expect(result.status).toBe(2);
    expect(result.stderr).toContain('cannot read/parse');
  });
});

// ── 6. check-layer-cycle.mjs ────────────────────────────────────────────────
describe('check-layer-cycle: a cruise that resolves nothing', () => {
  it('RED — an empty src exits non-zero instead of reporting no violation', () => {
    const app = makeApp({
      script: 'check-layer-cycle.mjs',
      dirs: ['src'],
      files: { '.dependency-cruiser.cjs': 'module.exports = { forbidden: [] };\n' },
    });

    const result = run(app);

    expect(result.status).not.toBe(0);
    expect(result.stdout + result.stderr).toContain('modules cruised under src/');
    expect(result.stdout).not.toContain('check-layer-cycle: OK');
  });
});
