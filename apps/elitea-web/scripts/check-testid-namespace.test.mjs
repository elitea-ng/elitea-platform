/**
 * RED/GREEN proof for check-testid-namespace.mjs (issue #312).
 *
 * The GREEN case is the REAL `src/` tree through the real script: 4700+ files
 * and 1100+ ids, so it also proves the floors are cleared by ordinary work.
 * Every RED case plants ONE violation in a miniature tree and spawns the real
 * script over it — the whole path, not the rule function alone: file walk,
 * comment stripping, value reading, floor check and exit code.
 *
 * The planted violations are the four shapes a reviewer would actually miss,
 * one per case, plus the two ways a gate can lie: an id inside a comment
 * (must NOT fail — it documents an id, it does not create one) and a selector
 * reading MUI's own `KeyboardArrowDownIcon` (must NOT fail — this repository
 * cannot rename an id it does not own).
 *
 * The floors get their own case: a tree with no sources must exit 2, because
 * a gate that measured nothing and printed a tick is the defect issue #528
 * named.
 */
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { afterAll, describe, expect, it } from 'vitest';

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const APP_ROOT = path.resolve(SCRIPT_DIR, '..');
const SCRIPT = path.join(SCRIPT_DIR, 'check-testid-namespace.mjs');

const workspace = mkdtempSync(path.join(tmpdir(), 'testid-namespace-'));
afterAll(() => rmSync(workspace, { recursive: true, force: true }));

let caseId = 0;

/**
 * A miniature `src/` holding `files`, padded to clear the gate's floors.
 *
 * The padding matters as much as the case: a fixture below the floor exits 2
 * for the WRONG reason, and a RED case that cannot tell 1 from 2 proves
 * nothing about the rule it claims to test.
 */
function tree(files, { pad = true } = {}) {
  const root = path.join(workspace, `case-${(caseId += 1)}`);
  mkdirSync(root, { recursive: true });
  for (const [name, content] of Object.entries(files)) {
    const file = path.join(root, name);
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(file, content);
  }
  if (pad) {
    for (let index = 0; index < 60; index += 1) {
      const ids = Array.from({ length: 4 }, (_, n) => `<b data-testid="pad-${index}-${n}" />`).join('');
      writeFileSync(path.join(root, `Pad${index}.tsx`), `export const P${index} = () => <>${ids}</>;\n`);
    }
  }
  return root;
}

function run(root) {
  const result = spawnSync(process.execPath, [SCRIPT, '--root', root, '--json'], {
    cwd: APP_ROOT,
    encoding: 'utf8',
  });
  return { status: result.status, report: JSON.parse(result.stdout) };
}

describe('check-testid-namespace: the real tree', () => {
  it('passes over src/ and states what it measured', () => {
    const result = spawnSync(process.execPath, [SCRIPT, '--json'], { cwd: APP_ROOT, encoding: 'utf8' });
    const report = JSON.parse(result.stdout);

    expect(report.violations).toEqual([]);
    expect(result.status).toBe(0);
    // The numbers are the proof the gate READ the tree. A gate that walks
    // nothing reports the same empty violation list.
    expect(report.files).toBeGreaterThan(1000);
    expect(report.ids).toBeGreaterThan(500);
  });
});

describe('check-testid-namespace: a planted violation', () => {
  const cases = [
    ['camelCase', '<b data-testid="firstMessageContent" />', 'firstMessageContent'],
    ['snake_case', '<b data-testid="agent_name_input" />', 'agent_name_input'],
    ['PascalCase through a testId prop', '<Row testId="PipelineSaveButton" />', 'PipelineSaveButton'],
    ['a template whose static text breaks the rule', '<b data-testid={`Agent_${id}-row`} />', 'Agent_${id}-row'],
    ['a same-file constant', "const ID = 'Agent.Row';\nexport const C = () => <b data-testid={ID} />;", 'Agent.Row'],
    [
      'the losing branch of a ternary',
      "<b data-testid={last ? 'chat-answer-content' : 'chatAnswerContent'} />",
      'chatAnswerContent',
    ],
  ];

  for (const [name, source, value] of cases) {
    it(`fails on ${name}`, () => {
      const { status, report } = run(tree({ 'Case.tsx': `export const C = () => (${source});\n` }));

      expect(status).toBe(1);
      expect(report.violations).toHaveLength(1);
      expect(report.violations[0].value).toBe(value);
      expect(report.violations[0].file).toContain('Case.tsx');
    });
  }
});

describe('check-testid-namespace: what is NOT an authored id', () => {
  it('ignores an id quoted in a comment', () => {
    const source = [
      '// The MUI icon renders `data-testid="CloseIcon"` outside production.',
      '/* Another one: data-testid="KeyboardArrowDownIcon". */',
      'export const C = () => <b data-testid="close-icon" />;',
    ].join('\n');

    const { status, report } = run(tree({ 'Case.tsx': `${source}\n` }));

    expect(report.violations).toEqual([]);
    expect(status).toBe(0);
  });

  it('ignores a selector that READS an id this repository does not own', () => {
    const source = [
      "export const find = (root) => root.querySelector('[data-testid=\"KeyboardArrowDownIcon\"]');",
      'export const hover = { [`&:hover [data-testid="${MENU}"]`]: { visibility: "visible" } };',
    ].join('\n');

    const { status, report } = run(tree({ 'Case.ts': `${source}\n` }));

    expect(report.violations).toEqual([]);
    expect(status).toBe(0);
  });

  it('counts a value it cannot resolve instead of dropping it', () => {
    const { status, report } = run(tree({ 'Case.tsx': 'export const C = ({ id }) => <b data-testid={id} />;\n' }));

    expect(status).toBe(0);
    expect(report.dynamic).toBe(1);
  });
});

describe('check-testid-namespace: the floors', () => {
  it('refuses a tree it found no sources in, rather than reporting it clean', () => {
    const { status, report } = run(tree({ 'notes.md': 'no sources here\n' }, { pad: false }));

    expect(status).toBe(2);
    expect(report.ok).toBe(false);
    expect(report.files).toBe(0);
  });
});
