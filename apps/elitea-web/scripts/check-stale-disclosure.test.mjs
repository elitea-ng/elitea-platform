/**
 * RED/GREEN proof for check-stale-disclosure.mjs (issue 621).
 *
 * The GREEN case is the real repository through the real scope list. The RED
 * cases run the real script against a scope list of fixtures written into a
 * temporary directory, so each one exercises the whole path — scope reading,
 * file reading, the probe over the real corpus, the exit code — and not just
 * the rule module.
 *
 * The RED fixture that matters is the first one: it is the defect this gate
 * was built for. It carries the claim api.gen.go used to carry, "centry
 * .audit_events ... Never emitted today", with the probe that falsifies it.
 * internal/audit/postgres.go writes that table, so the gate must fail.
 */
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { afterAll, describe, expect, it } from 'vitest';

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const APP_ROOT = path.resolve(SCRIPT_DIR, '..');
const REPO_ROOT = path.resolve(APP_ROOT, '../..');
const SCRIPT = path.join(SCRIPT_DIR, 'check-stale-disclosure.mjs');

const workspace = mkdtempSync(path.join(tmpdir(), 'stale-disclosure-'));
afterAll(() => rmSync(workspace, { recursive: true, force: true }));

let caseId = 0;

/**
 * Five well-formed claim/probe pairs, then a gap wider than the attachment
 * window.
 *
 * The gate states three floors — files, claims and probes — and fails below
 * any of them, because a scope that has collapsed must not report a clean
 * tree. A fixture therefore has to carry a plausible number of both. The gap
 * keeps these probes from reaching the case under test: they attach forward
 * only, and DEFAULT_WINDOW is 15 lines.
 */
const PADDING = [
  ...[1, 2, 3, 4, 5].flatMap((n) => [
    `DISCLOSURE-CHECK: absent \`zz-no-such-literal-${n}\` in services/elitea-main/internal/audit/event.go`,
    `NO PRODUCER for padding ${n}.`,
  ]),
  ...Array.from({ length: 20 }, () => ''),
].join('\n');

/**
 * Write one fixture file plus a scope list naming it, and run the gate.
 *
 * The fixture is written INSIDE the repository so the scope list can name it
 * with a repo-relative path, which is the only shape the script accepts. It is
 * removed again as soon as the run ends.
 */
function runAgainst(caseText) {
  const fixtureText = `${PADDING}\n${caseText}`;
  caseId += 1;
  const fixtureName = `.stale-disclosure-fixture-${process.pid}-${caseId}.txt`;
  const fixturePath = path.join(REPO_ROOT, fixtureName);
  const scopePath = path.join(workspace, `scope-${caseId}.txt`);
  writeFileSync(fixturePath, fixtureText, 'utf8');
  // Two entries: the fixture, and one real file, so the scope floor of 2 is met
  // by the same list every real run uses.
  writeFileSync(scopePath, `${fixtureName}\nservices/elitea-main/internal/audit/event.go\n`, 'utf8');
  try {
    return spawnSync(process.execPath, [SCRIPT, '--scope', scopePath], {
      cwd: APP_ROOT,
      encoding: 'utf8',
    });
  } finally {
    rmSync(fixturePath, { force: true });
  }
}

describe('GREEN — the real repository through the real scope list', () => {
  it('exits 0 and states what it measured', () => {
    const result = spawnSync(process.execPath, [SCRIPT], { cwd: APP_ROOT, encoding: 'utf8' });
    expect(result.stdout).toContain('check-stale-disclosure: OK');
    expect(result.stdout).toMatch(/measured \d+ disclosure claims in those files \(floor 5\)/);
    expect(result.status).toBe(0);
  });

  it('reports the same verdict as --json', () => {
    const result = spawnSync(process.execPath, [SCRIPT, '--json'], { cwd: APP_ROOT, encoding: 'utf8' });
    expect(result.status).toBe(0);
    const parsed = JSON.parse(result.stdout);
    expect(parsed.ok).toBe(true);
    expect(parsed.violations).toEqual([]);
    expect(parsed.claims).toBeGreaterThanOrEqual(5);
  });
});

describe('RED — a stale claim must fail the build', () => {
  it('fails the claim api.gen.go used to carry, because the writer now exists', () => {
    const result = runAgainst(
      [
        'DISCLOSURE-CHECK: absent `INSERT INTO centry.audit_events` in services/elitea-main/internal/audit',
        'centry.audit_events is READ-ONLY from this service. Never emitted today.',
        '',
      ].join('\n'),
    );
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('STALE');
    expect(result.stderr).toContain('INSERT INTO centry.audit_events');
  });

  it('fails a claim that carries no probe', () => {
    const result = runAgainst('this surface is NOT PORTED to the Go stack.\n');
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('UNVERIFIED');
  });

  it('fails a probe whose path matches no file', () => {
    const result = runAgainst(
      ['DISCLOSURE-CHECK: absent `anything` in services/elitea-main/no-such-directory', 'NO PRODUCER.', ''].join('\n'),
    );
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('BLIND');
  });

  it('fails a probe nobody can parse', () => {
    const result = runAgainst(['DISCLOSURE-CHECK: absent nothing-in-backticks', 'NO PRODUCER.', ''].join('\n'));
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('MALFORMED');
  });

  it('exits 2 when the scope list names a file that is not there', () => {
    const scopePath = path.join(workspace, 'scope-missing.txt');
    writeFileSync(scopePath, 'services/elitea-main/no-such-file.go\n', 'utf8');
    const result = spawnSync(process.execPath, [SCRIPT, '--scope', scopePath], { cwd: APP_ROOT, encoding: 'utf8' });
    // The floor fires first: one entry is below the floor of two, and a scope
    // that collapsed to nothing must not report a clean tree.
    expect(result.status).toBe(2);
    expect(result.stderr).toContain('the subject set is empty or too small');
  });
});
