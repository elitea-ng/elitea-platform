#!/usr/bin/env node
/**
 * check-gates-selftest.mjs — the RED/GREEN proof for the quality-gate layer
 * (unit F2; D2 acceptance: "every §3.4 rule ships a failing fixture and a
 * passing fixture proving enforcement on this stack").
 *
 * For every enforced rule this runs its FAILING fixture (must be rejected —
 * RED) and its PASSING fixture (must be accepted — GREEN) through the REAL
 * enforcement mechanism: oxlint with the production .oxlintrc.json,
 * dependency-cruiser with the production .dependency-cruiser.cjs, knip,
 * scripts/check-budgets.mjs, scripts/theme-gate.mjs, and vitest booting the
 * production src/test/setup.ts. A failing fixture that passes is a defect —
 * this script IS the mutation proof for the rule layer.
 *
 * oxlint mechanics: each fixture case is a mini-app (<case>/src/... +
 * tsconfig.json). oxlint runs with cwd=<case> so the path-scoped overrides
 * (sanctioned files like src/shared/api/http.ts) resolve exactly as in the
 * real tree. The production config is copied per-case with jsPlugins paths
 * absolutized and (except the TS-AWARE case) options.typeAware=false —
 * oxlint-tsgolint 7.0.2001 panics on tsconfig discovery when the config file
 * and cwd diverge (typescript-go vfs wants absolute paths), so type-aware
 * proof runs in its own case where config+tsconfig sit in cwd, the layout the
 * production run uses too.
 */
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import { stripJsonc } from './lib/jsonc.mjs';
import { checkHandlerSource } from './lib/mock-rules-core.mjs';
import { checkSpecExamples } from './lib/spec-examples-core.mjs';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const yaml = createRequire(import.meta.url)('js-yaml');
const APP = resolve(SCRIPT_DIR, '..');
const FIXTURES = join(APP, 'tools', 'lint-rules', 'fixtures');
const BIN = (name) => join(APP, 'node_modules', '.bin', name);

// The production config carries comments. `stripJsonc` moved to
// scripts/lib/jsonc.mjs with issue #528 — scripts/check-dead-code.mjs reads
// knip.json with the same scanner, and importing it from here would run this
// whole self-test as a side effect.
const productionConfig = JSON.parse(stripJsonc(readFileSync(join(APP, '.oxlintrc.json'), 'utf8')));

function writeCaseConfig(caseDir, { typeAware }) {
  const config = structuredClone(productionConfig);
  delete config.$schema;
  delete config.ignorePatterns; // fixtures are the lint target here
  config.jsPlugins = config.jsPlugins.map((p) => (p.startsWith('./') ? join(APP, p) : p));
  config.options = { ...config.options, typeAware };
  const file = join(caseDir, '.oxlintrc.gen.json');
  writeFileSync(file, JSON.stringify(config, null, 2));
  return file;
}

// The generated config is addressed RELATIVE to the case cwd: with an
// absolute -c path, oxlint-tsgolint resolves linted files against the config
// directory and panics in typescript-go's vfs on the resulting relative
// tsconfig probes (verified 2026-07-26). Relative config + cwd=case is also
// exactly the production layout (.oxlintrc.json next to src/).
const CASE_CONFIG = '.oxlintrc.gen.json';

// ── result collection ──
const rows = [];
let defects = 0;

function record(id, mechanism, redOk, greenOk, note = '') {
  const ok = redOk && greenOk;
  if (!ok) defects++;
  rows.push({ id, mechanism, red: redOk ? 'RED✔' : 'RED✘', green: greenOk ? 'GREEN✔' : 'GREEN✘', note });
}

function recordDelegated(id, mechanism, owner) {
  rows.push({ id, mechanism, red: '—', green: '—', note: `DELEGATED to ${owner}` });
}
/** A rule that no tool enforces. Listed so the gap stays visible, not counted as delegated. */
function recordUngated(id, mechanism, reason) {
  rows.push({ id, mechanism, red: 'n/a', green: 'n/a', note: `NOT GATED — ${reason}` });
}

// ── section 1: oxlint rule pairs ──
const OXLINT_CASES = [
  ['R-A1', 'oxlint eslint/no-restricted-globals (fetch)', 'eslint(no-restricted-globals)'],
  ['R-A2', 'oxlint eslint/no-restricted-imports (axios)', 'eslint(no-restricted-imports)'],
  ['R-A3', 'oxlint eslint/no-restricted-imports (socket.io-client)', 'eslint(no-restricted-imports)'],
  ['R-A1-MEMBER', 'oxlint eslint/no-restricted-properties (window/globalThis/self.fetch — member-expression half of R-A1)', 'eslint(no-restricted-properties)'],
  ['R-A4', 'oxlint eslint/no-restricted-globals (XMLHttpRequest)', 'eslint(no-restricted-globals)'],
  ['R-A4-MEMBER', 'oxlint eslint/no-restricted-properties (window/globalThis/self.XMLHttpRequest — member-expression half of R-A4)', 'eslint(no-restricted-properties)'],
  ['R-A6', 'elitea/no-adhoc-envelope-unwrap (jsPlugins; shared/api/unwrap.ts override)', 'elitea(no-adhoc-envelope-unwrap)'],
  ['STOR-1', 'oxlint eslint/no-restricted-globals (localStorage, §5.4)', 'eslint(no-restricted-globals)'],
  ['STOR-2', 'elitea/no-raw-webstorage (jsPlugins; member-expression half of §5.4, issue #22)', 'elitea(no-raw-webstorage)'],
  ['R-T4', 'oxlint eslint/no-restricted-imports (useTheme importNames)', 'eslint(no-restricted-imports)'],
  ['R-I1', 'oxlint eslint/no-restricted-imports (@mui/icons-material barrel)', 'eslint(no-restricted-imports)'],
  ['R-T1', 'elitea/no-raw-color (jsPlugins)', 'elitea(no-raw-color)'],
  ['R-T2', 'elitea/no-mode-branch (jsPlugins; theme-gate grep is backstop)', 'elitea(no-mode-branch)'],
  ['R-T5', 'elitea/no-important-sx (jsPlugins)', 'elitea(no-important-sx)'],
  ['R-T6', 'elitea/no-mui-internal-selector (jsPlugins)', 'elitea(no-mui-internal-selector)'],
  ['R-T7', 'elitea/no-theme-palette (jsPlugins)', 'elitea(no-theme-palette)'],
  ['R-T9', 'elitea/raw-px-spacing (jsPlugins)', 'elitea(raw-px-spacing)'],
  ['R-T10', 'elitea/ad-hoc-radius (jsPlugins)', 'elitea(ad-hoc-radius)'],
  ['R-T11', 'elitea/ad-hoc-font-size (jsPlugins)', 'elitea(ad-hoc-font-size)'],
  // Typography spec rev. 2 §4.1: one RED fixture per value shape the
  // hardened R-T11 reports, each against the same GREEN set of allowed shapes.
  ['R-T11-VAR', 'elitea/ad-hoc-font-size — var(--el-font-*) shorthand used as a size', 'elitea(ad-hoc-font-size)'],
  ['R-T11-TEMPLATE', 'elitea/ad-hoc-font-size — template-literal size', 'elitea(ad-hoc-font-size)'],
  ['R-T11-CALL', 'elitea/ad-hoc-font-size — computed size (pxToRem/calc helper)', 'elitea(ad-hoc-font-size)'],
  ['R-T11-IDENT', 'elitea/ad-hoc-font-size — identifier size', 'elitea(ad-hoc-font-size)'],
  ['R-T11-COND', 'elitea/ad-hoc-font-size — conditional with an ad-hoc operand', 'elitea(ad-hoc-font-size)'],
  ['R-T11-STOCK', 'elitea/ad-hoc-font-size — member read of a MUI stock variant', 'elitea(ad-hoc-font-size)'],
  // GREEN here is the proof that `oxlint-disable-next-line
  // elitea/ad-hoc-font-size` suppresses through the jsPlugins bridge.
  ['R-T11-DISABLE', 'elitea/ad-hoc-font-size — per-line oxlint-disable passes the jsPlugins bridge', 'elitea(ad-hoc-font-size)'],
  ['R-T13', 'elitea/no-stock-typography-variant (jsPlugins)', 'elitea(no-stock-typography-variant)'],
  ['R-L4', 'elitea/no-export-all (jsPlugins)', 'elitea(no-export-all)'],
  ['R-S2', 'elitea/no-module-scope-store (jsPlugins)', 'elitea(no-module-scope-store)'],
  ['R-M1', 'elitea/no-vi-mock (jsPlugins; __mocks__ override)', 'elitea(no-vi-mock)'],
  ['R-T3', 'eslint-plugin-i18next via jsPlugins (D2 bridge)', 'i18next(no-literal-string)'],
  ['R-C1', 'oxlint jsx-a11y at error', 'jsx-a11y('],
  ['HOOKS-DEPS', 'oxlint react/exhaustive-deps at error (D2)', 'react-hooks(exhaustive-deps)'],
  ['HOOKS-RULES', 'oxlint react/rules-of-hooks at error', 'react-hooks(rules-of-hooks)'],
  ['COMPLEXITY', 'oxlint eslint/complexity 12 (§3.5)', 'eslint(complexity)'],
  ['MAXLINES', 'oxlint eslint/max-lines 400 (§3.5; pass = exempt test file)', 'eslint(max-lines)'],
  ['EMPTY-CATCH', 'oxlint eslint/no-empty, allowEmptyCatch:false (§3.6)', 'eslint(no-empty)'],
  ['TS-AWARE', 'oxlint-tsgolint typescript/no-floating-promises (type-aware set)', 'typescript(no-floating-promises)'],
];

// NOTE: no --no-ignore. The generated config carries no ignorePatterns (they
// are stripped), and the flag flips oxlint-tsgolint into the relative-path
// codepath that panics typescript-go's vfs (verified 2026-07-26).
function runOxlintCase(caseDir) {
  return spawnSync(BIN('oxlint'), ['-c', CASE_CONFIG, 'src'], {
    cwd: caseDir,
    encoding: 'utf8',
  });
}

for (const [id, mechanism, tag] of OXLINT_CASES) {
  const base = join(FIXTURES, 'rules', id);
  const typeAware = id === 'TS-AWARE';
  const failDir = join(base, 'fail');
  const passDir = join(base, 'pass');
  const failCfg = writeCaseConfig(failDir, { typeAware });
  const passCfg = writeCaseConfig(passDir, { typeAware });
  try {
    const fail = runOxlintCase(failDir);
    const failOut = fail.stdout + fail.stderr;
    const redOk = fail.status !== 0 && failOut.includes(tag);
    const pass = runOxlintCase(passDir);
    const passOut = pass.stdout + pass.stderr;
    const greenOk = pass.status === 0 && !/: (error|warning) /.test(passOut);
    record(id, mechanism, redOk, greenOk);
    if (!redOk) console.error(`\n[${id}] RED did not prove (exit=${fail.status}, expected tag "${tag}"):\n${failOut}`);
    if (!greenOk) console.error(`\n[${id}] GREEN did not prove (exit=${pass.status}):\n${passOut}`);
  } finally {
    rmSync(failCfg, { force: true });
    rmSync(passCfg, { force: true });
  }
}

// ── section 2: dependency-cruiser (R-L1/R-L2/R-L3 — enforcement of record, D2) ──
{
  const DC_RULES = [
    'no-upward-from-shared',
    'no-sideways-features',
    'no-sideways-entities',
    'no-circular',
    'no-deep-slice-import',
    'no-deep-slice-import-cross-slice',
    'not-to-unresolvable',
  ];
  const run = (dir) =>
    spawnSync(BIN('depcruise'), ['src', '--config', join(APP, '.dependency-cruiser.cjs')], {
      cwd: dir,
      encoding: 'utf8',
    });
  const bad = run(join(FIXTURES, 'depcruise', 'bad'));
  const badOut = bad.stdout + bad.stderr;
  const missing = DC_RULES.filter((rule) => !badOut.includes(`error ${rule}`));
  const good = run(join(FIXTURES, 'depcruise', 'good'));
  const redOk = bad.status !== 0 && missing.length === 0;
  const greenOk = good.status === 0;
  record('R-L1', 'dependency-cruiser no-upward-from-* + no-sideways-*', redOk, greenOk);
  record('R-L2', 'dependency-cruiser no-circular', redOk, greenOk);
  record('R-L3', 'dependency-cruiser no-deep-slice-import(+cross-slice) + not-to-unresolvable', redOk, greenOk);
  if (missing.length > 0) console.error(`\n[depcruise] rules that did not fire in the bad tree: ${missing.join(', ')}\n${badOut}`);
  if (!greenOk) console.error(`\n[depcruise] good tree not clean:\n${good.stdout}${good.stderr}`);
}

// ── section 3: knip (R-D1) ──
{
  const run = (dir) => spawnSync(BIN('knip'), ['--max-issues', '0'], { cwd: dir, encoding: 'utf8' });
  const bad = run(join(FIXTURES, 'knip', 'bad'));
  const badOut = bad.stdout + bad.stderr;
  const redOk = bad.status !== 0 && badOut.includes('Unused files') && badOut.includes('Unused exports');
  const good = run(join(FIXTURES, 'knip', 'good'));
  record('R-D1', 'knip --max-issues 0 (dead files + dead exports)', redOk, good.status === 0);
  if (!redOk) console.error(`\n[knip] bad tree did not prove:\n${badOut}`);
}

// ── section 4: §3.5 budgets (scripts/check-budgets.mjs) ──
{
  const run = (dir) =>
    spawnSync(process.execPath, [join(SCRIPT_DIR, 'check-budgets.mjs'), '--root', dir], { encoding: 'utf8' });
  const BUDGET_RULES = ['file-length', 'component-props', 'use-effects', 'hook-deps', 'slice-public-api'];
  const bad = run(join(FIXTURES, 'budgets', 'bad'));
  const badOut = bad.stdout + bad.stderr;
  const missing = BUDGET_RULES.filter((rule) => !badOut.includes(`BUDGET ${rule}:`));
  const good = run(join(FIXTURES, 'budgets', 'good'));
  const greenOk = good.status === 0;
  for (const rule of BUDGET_RULES) {
    record(
      `BUDGET/${rule}`,
      'scripts/check-budgets.mjs (§3.5; good tree holds the exact boundary + exemptions)',
      bad.status !== 0 && badOut.includes(`BUDGET ${rule}:`),
      greenOk,
    );
  }
  if (missing.length > 0) console.error(`\n[budgets] rules that did not fire: ${missing.join(', ')}\n${badOut}`);
  if (!greenOk) console.error(`\n[budgets] good tree not clean:\n${good.stdout}${good.stderr}`);
}

// ── section 5: §4.6 theme gate (scripts/theme-gate.mjs) ──
{
  const run = (dir) =>
    spawnSync(process.execPath, [join(SCRIPT_DIR, 'theme-gate.mjs'), '--root', dir], { encoding: 'utf8' });
  const CHECKS = ['1-raw-colors', '2-mode-branches', '3-theme-palette', '4-mui-selectors', '5-forked-assets', '6-external-origins', '8-subapp-strings'];
  const bad = run(join(FIXTURES, 'theme-gate', 'bad'));
  const badOut = bad.stdout + bad.stderr;
  const good = run(join(FIXTURES, 'theme-gate', 'good'));
  const goodOut = good.stdout + good.stderr;
  const greenOk = good.status === 0;
  for (const check of CHECKS) {
    record(
      `THEME/${check}`,
      'scripts/theme-gate.mjs (§4.6; check 1 runs elitea/no-raw-color via oxlint)',
      bad.status !== 0 && badOut.includes(`theme-gate ${check}: FAIL`),
      greenOk && goodOut.includes(`theme-gate ${check}: PASS`),
    );
  }
  recordDelegated('THEME/7-brand-pack', 'vitest brand-pack contract test (§4.6 check 7; theme-gate invokes when present)', 'unit T1');
}

// ── section 6: R-M5 (vitest fixtures booting the REAL src/test/setup.ts) ──
{
  const run = (file) =>
    spawnSync(
      BIN('vitest'),
      ['run', '--config', join(SCRIPT_DIR, 'selftest', 'vitest.fixtures.config.mts'), file],
      { cwd: APP, encoding: 'utf8' },
    );
  const rm5Fail = run('tools/lint-rules/fixtures/msw/rm5-fail.test.ts');
  const rm5Pass = run('tools/lint-rules/fixtures/msw/rm5-pass.test.ts');
  // The RED run must fail BECAUSE of the fence, not for any other reason: the
  // fixture's own server answers 200, and msw names the option it enforced.
  const rm5Fenced = rm5Fail.status !== 0 && `${rm5Fail.stdout}${rm5Fail.stderr}`.includes('"onUnhandledRequest"');
  record(
    'R-M5',
    "msw onUnhandledRequest:'error' in src/test/setup.ts",
    rm5Fenced,
    rm5Pass.status === 0,
  );
  if (!rm5Fenced) console.error(`\n[R-M5] unmocked request was NOT fenced:\n${rm5Fail.stdout}${rm5Fail.stderr}`);
  if (rm5Pass.status !== 0) console.error(`\n[R-M5] handled request failed:\n${rm5Pass.stdout}${rm5Pass.stderr}`);
}

// ── section 7: R-M2 (scripts/lib/mock-rules-core.mjs on fixtures) ──
{
  const badSource = readFileSync(join(FIXTURES, 'msw', 'handlers-bad.ts'), 'utf8');
  const goodSource = readFileSync(join(FIXTURES, 'msw', 'handlers-good.ts'), 'utf8');
  const rm2Red = checkHandlerSource('handlers-bad.ts', badSource);
  const rm2Green = checkHandlerSource('handlers-good.ts', goodSource);
  record(
    'R-M2',
    'check-handlers.mjs (bodies: generated mock or typed constant; no inline literal, no JSON)',
    rm2Red.length >= 2,
    rm2Green.length === 0,
  );
}

// ── section 7b: R-M3 (scripts/lib/spec-examples-core.mjs on fixture specs) ──
// R-M3 was a runtime zod check of each hand-written JSON fixture. Those
// fixtures are gone: a mock body is now a contract example (checked here,
// against its schema) or a typed constant (checked by tsc).
{
  const load = (name) => yaml.load(readFileSync(join(FIXTURES, 'spec-examples', name), 'utf8'), { schema: yaml.CORE_SCHEMA });
  const rm3Red = checkSpecExamples(load('bad.yaml'));
  const rm3Green = checkSpecExamples(load('good.yaml'));
  record(
    'R-M3',
    'check-spec-examples.mjs (v2.yaml example/examples vs their schema, Ajv)',
    rm3Red.findings.length > 0,
    rm3Green.checked > 0 && rm3Green.findings.length === 0,
  );
}

// ── section 8: rules whose enforcement mechanism belongs to a later unit ──
recordDelegated('R-A5', 'scripts/check-endpoint-manifest.mjs (endpoint manifest vs shared/api/endpoints/)', 'units S4+W2');
recordDelegated('R-S1', 'S-unit acceptance tests asserting store shape (no query-derivable state)', 'S-units, review');
recordDelegated('R-C2', 'theme-level variantMapping config + emitted-tag unit test; Storybook a11y at error', 'units T1/S1');
recordDelegated('BUDGET/prop-drill-3', '§3.5 prop-drill depth ≤3 — needs cross-module data flow; R-L3 structure narrows it', 'review');
// Was "DELEGATED to units R1 + uictl". Neither ever measured it: uictl only
// diffed route patterns, and it was deleted with the apps/elitea-ui submodule.
// The nearest real guard is the per-route chunk byte budget in
// scripts/check-bundle-budget.mjs (build job), which bounds size, not count.
recordUngated('BUDGET/route-closure-250', '§3.5 transitive import closure ≤250 per route entry', 'no tool counts it; check-bundle-budget.mjs bounds route-chunk bytes instead');

// ── report ──
const width = { id: 22, mech: 78, red: 6, green: 7 };
console.log('\n══ F2 gate self-test — every enforced rule with its RED/GREEN fixture proof ══\n');
console.log(
  `${'RULE'.padEnd(width.id)}${'MECHANISM'.padEnd(width.mech)}${'RED'.padEnd(width.red)}${'GREEN'.padEnd(width.green)}NOTE`,
);
for (const row of rows) {
  console.log(
    `${row.id.padEnd(width.id)}${row.mechanism.slice(0, width.mech - 2).padEnd(width.mech)}${row.red.padEnd(width.red)}${row.green.padEnd(width.green)}${row.note}`,
  );
}
const proved = rows.filter((r) => r.red === 'RED✔' && r.green === 'GREEN✔').length;
const delegated = rows.filter((r) => r.red === '—').length;
const ungated = rows.filter((r) => r.red === 'n/a').length;
console.log(
  `\n${proved} rule(s) proved RED+GREEN, ${delegated} delegated to later units, ${ungated} not gated, ${defects} defect(s).`,
);
if (defects > 0) {
  console.error('check-gates-selftest: FAIL — a rule whose failing fixture passes is a defect (D2).');
  process.exit(1);
}
console.log('check-gates-selftest: OK');
