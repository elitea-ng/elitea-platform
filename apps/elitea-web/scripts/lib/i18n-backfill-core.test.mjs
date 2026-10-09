import { describe, expect, it } from 'vitest';

import { extractCallSites, isDesktopOnlyPath, planBackfill, planCatalogueSplit, routeNewKeys } from './i18n-backfill-core.mjs';

/** Table-driven coverage of every decision this module makes (same
 * 100%-of-decision-logic floor F2's scripts/lib/*-core.mjs modules carry). */

describe('extractCallSites', () => {
  it('captures a plain string-literal (key, fallback) pair, real import source', () => {
    const source = `
      import { t } from '@/shared/i18n';
      function Widget() {
        return t('widgets.save', 'Save');
      }
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([{ key: 'widgets.save', fallback: 'Save', filename: 'Widget.tsx', line: 4 }]);
    expect(flagged).toEqual([]);
  });

  it('does NOT capture the removed pre-S8 stub source (@/shared/ui/lib/t), which no longer exists (#45)', () => {
    // Issue #45 migrated all 79 importers to `@/shared/i18n` and deleted
    // `src/shared/ui/lib/t.ts`. Keeping it as an extraction source would let
    // a reintroduced stub import silently satisfy the --check gate again.
    const source = `
      import { t } from '@/shared/ui/lib/t';
      t('legacy.stub.label', 'Label');
    `;
    const { entries, flagged } = extractCallSites('Legacy.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('captures a template-literal fallback with no ${} expressions as a static string', () => {
    const source = `
      import { t } from '@/shared/i18n';
      t('widgets.title', \`Static Title\`);
    `;
    const { entries } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([{ key: 'widgets.title', fallback: 'Static Title', filename: 'Widget.tsx', line: 3 }]);
  });

  it('does NOT resolve the removed stub via a relative path either (`../lib/t`, the spelling 42 shared/ui/** files used pre-#45)', () => {
    const source = `
      import { t } from '../lib/t';
      t('shared.ui.commonNumberField.mustBeGreaterThan', 'Value must be greater than {{min}}');
    `;
    const { entries, flagged } = extractCallSites('src/shared/ui/CommonNumberField/CommonNumberField.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('resolves a relative import of the real i18n barrel from a sibling directory', () => {
    const source = `
      import { t } from '../i18n';
      t('deep.nested', 'Nested');
    `;
    const { entries } = extractCallSites('src/shared/settings/DeepFile.tsx', source);
    expect(entries).toEqual([{ key: 'deep.nested', fallback: 'Nested', filename: 'src/shared/settings/DeepFile.tsx', line: 3 }]);
  });

  it('ignores a relative import that resolves to neither real target', () => {
    const source = `
      import { t } from '../lib/unrelated';
      t('not.tracked', 'Nope');
    `;
    const { entries, flagged } = extractCallSites('src/shared/ui/CommonNumberField/CommonNumberField.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('resolves an aliased local binding (import { t as translate })', () => {
    const source = `
      import { t as translate } from '@/shared/i18n';
      translate('widgets.aliased', 'Aliased');
    `;
    const { entries } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([{ key: 'widgets.aliased', fallback: 'Aliased', filename: 'Widget.tsx', line: 3 }]);
  });

  it('resolves both a named export and t on the same import statement (import { i18n, t } from ...)', () => {
    const source = `
      import { i18n, t } from '@/shared/i18n';
      t('widgets.both', 'Both');
      i18n.changeLanguage('en');
    `;
    const { entries } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([{ key: 'widgets.both', fallback: 'Both', filename: 'Widget.tsx', line: 3 }]);
  });

  it('flags a template-literal-with-expressions fallback as interpolated-fallback, key still known', () => {
    const source = `
      import { t } from '@/shared/i18n';
      function f(currentPage, totalPages) {
        return t('entities.secret.table.pageInfo', \`Page \${currentPage} of \${totalPages}\`);
      }
    `;
    const { entries, flagged } = extractCallSites('SecretsTable.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([
      {
        filename: 'SecretsTable.tsx',
        line: 4,
        key: 'entities.secret.table.pageInfo',
        reason: 'interpolated-fallback',
        detail: "t('entities.secret.table.pageInfo', `Page ${currentPage} of ${totalPages}`)",
      },
    ]);
  });

  it('flags a non-literal, non-template fallback (bare identifier) as interpolated-fallback', () => {
    const source = `
      import { t } from '@/shared/i18n';
      function f(dynamicFallback) {
        return t('widgets.dynamic', dynamicFallback);
      }
    `;
    const { flagged } = extractCallSites('Widget.tsx', source);
    expect(flagged).toEqual([
      { filename: 'Widget.tsx', line: 4, key: 'widgets.dynamic', reason: 'interpolated-fallback', detail: "t('widgets.dynamic', dynamicFallback)" },
    ]);
  });

  it('flags a call missing the fallback argument entirely as interpolated-fallback', () => {
    const source = `
      import { t } from '@/shared/i18n';
      t('widgets.onlyKey');
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([
      { filename: 'Widget.tsx', line: 3, key: 'widgets.onlyKey', reason: 'interpolated-fallback', detail: "t('widgets.onlyKey')" },
    ]);
  });

  it('flags a non-literal key (first argument not a string literal) as dynamic-key', () => {
    const source = `
      import { t } from '@/shared/i18n';
      function f(dynamicKey) {
        return t(dynamicKey, 'Fallback');
      }
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([
      { filename: 'Widget.tsx', line: 4, reason: 'dynamic-key', detail: "t(dynamicKey, 'Fallback')" },
    ]);
  });

  it('flags a call with zero arguments as dynamic-key (no key to classify at all)', () => {
    const source = `
      import { t } from '@/shared/i18n';
      t();
    `;
    const { flagged } = extractCallSites('Widget.tsx', source);
    expect(flagged).toEqual([{ filename: 'Widget.tsx', line: 3, reason: 'dynamic-key', detail: 't()' }]);
  });

  it('ignores a call to an unrelated identifier named "t" that was never imported from a real source', () => {
    const source = `
      function t(a, b) { return b; }
      t('not.a.real.key', 'Not tracked');
    `;
    const { entries, flagged } = extractCallSites('Local.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('ignores an import of `t` from an unrelated module', () => {
    const source = `
      import { t } from 'some-other-library';
      t('not.tracked', 'Nope');
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('ignores a namespace import from a real source (no named `t` binding to resolve)', () => {
    const source = `
      import * as i18nNs from '@/shared/i18n';
      i18nNs.t('not.tracked', 'Nope');
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('ignores a member-expression call even when the property name matches a bound t name', () => {
    const source = `
      import { t } from '@/shared/i18n';
      const obj = { t: () => {} };
      obj.t('not.tracked', 'Nope');
    `;
    const { entries, flagged } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toEqual([]);
  });

  it('returns no entries/flags for a file that never imports t at all', () => {
    const source = `
      export function helper() { return 1 + 1; }
    `;
    expect(extractCallSites('Helper.ts', source)).toEqual({ entries: [], flagged: [] });
  });

  it('surfaces a parse error as a flagged entry instead of throwing', () => {
    const source = 'export const x = {{{ this is not valid syntax';
    const { entries, flagged } = extractCallSites('Broken.tsx', source);
    expect(entries).toEqual([]);
    expect(flagged).toHaveLength(1);
    expect(flagged[0]).toMatchObject({ filename: 'Broken.tsx', line: 1, reason: 'parse-error' });
    expect(flagged[0].detail).toBeTruthy();
  });

  it('captures multiple call sites across a file with mixed real and non-t calls', () => {
    const source = `
      import { t } from '@/shared/i18n';
      console.log('side effect');
      function Widget() {
        return t('widgets.a', 'A') + t('widgets.b', 'B');
      }
    `;
    const { entries } = extractCallSites('Widget.tsx', source);
    expect(entries).toEqual([
      { key: 'widgets.a', fallback: 'A', filename: 'Widget.tsx', line: 5 },
      { key: 'widgets.b', fallback: 'B', filename: 'Widget.tsx', line: 5 },
    ]);
  });
});

describe('planBackfill', () => {
  it('returns empty plan for no entries and no existing keys', () => {
    expect(planBackfill({}, [])).toEqual({ toAdd: {}, conflicts: [], drifted: [] });
  });

  it('adds a new key when every call site agrees on the exact same fallback', () => {
    const entries = [
      { key: 'widgets.save', fallback: 'Save', filename: 'A.tsx', line: 1 },
      { key: 'widgets.save', fallback: 'Save', filename: 'B.tsx', line: 2 },
    ];
    const { toAdd, conflicts, drifted } = planBackfill({}, entries);
    expect(toAdd).toEqual({ 'widgets.save': 'Save' });
    expect(conflicts).toEqual([]);
    expect(drifted).toEqual([]);
  });

  it('reports a hard conflict when call sites for a new key disagree, never picking one', () => {
    const entries = [
      { key: 'chat-participants.warning.remoteMcpExpired', fallback: 'Remote MCP session expired. ', filename: 'ParticipantWarning.tsx', line: 102 },
      { key: 'chat-participants.warning.remoteMcpExpired', fallback: 'Remote MCP session expired.', filename: 'ParticipantWarning.tsx', line: 110 },
    ];
    const { toAdd, conflicts } = planBackfill({}, entries);
    expect(toAdd).toEqual({});
    expect(conflicts).toEqual([
      {
        key: 'chat-participants.warning.remoteMcpExpired',
        variants: [
          { fallback: 'Remote MCP session expired. ', sites: [{ filename: 'ParticipantWarning.tsx', line: 102 }] },
          { fallback: 'Remote MCP session expired.', sites: [{ filename: 'ParticipantWarning.tsx', line: 110 }] },
        ],
      },
    ]);
  });

  it('never overwrites an existing key even when a call site fallback matches it exactly', () => {
    const existingEn = { 'widgets.save': 'Save' };
    const entries = [{ key: 'widgets.save', fallback: 'Save', filename: 'A.tsx', line: 1 }];
    const { toAdd, drifted } = planBackfill(existingEn, entries);
    expect(toAdd).toEqual({});
    expect(drifted).toEqual([]);
  });

  it('surfaces shipped-key-text-drifted when a call site fallback no longer matches the shipped value', () => {
    const existingEn = { 'entities.projectContext.content.saveFailed': 'Failed to save Project Context' };
    const entries = [
      { key: 'entities.projectContext.content.saveFailed', fallback: 'Could not save', filename: 'ProjectContextBody.tsx', line: 205 },
    ];
    const { toAdd, drifted } = planBackfill(existingEn, entries);
    expect(toAdd).toEqual({});
    expect(drifted).toEqual([
      {
        key: 'entities.projectContext.content.saveFailed',
        shipped: 'Failed to save Project Context',
        variants: [{ fallback: 'Could not save', sites: [{ filename: 'ProjectContextBody.tsx', line: 205 }] }],
      },
    ]);
  });

  it('groups repeated (key, fallback) pairs into one variant with multiple sites', () => {
    const entries = [
      { key: 'widgets.save', fallback: 'Save', filename: 'A.tsx', line: 1 },
      { key: 'widgets.save', fallback: 'Save', filename: 'B.tsx', line: 2 },
      { key: 'widgets.save', fallback: 'Save', filename: 'C.tsx', line: 3 },
    ];
    const { conflicts } = planBackfill({}, entries);
    expect(conflicts).toEqual([]);
  });

  it('handles a mix of new-key add, new-key conflict, no-drift existing, and drifted existing in one pass', () => {
    const existingEn = {
      'a.clean': 'Clean',
      'a.drifted': 'Old Text',
    };
    const entries = [
      { key: 'a.toAdd', fallback: 'Add me', filename: 'X.tsx', line: 1 },
      { key: 'a.conflict', fallback: 'One', filename: 'X.tsx', line: 2 },
      { key: 'a.conflict', fallback: 'Two', filename: 'Y.tsx', line: 3 },
      { key: 'a.clean', fallback: 'Clean', filename: 'X.tsx', line: 4 },
      { key: 'a.drifted', fallback: 'New Text', filename: 'X.tsx', line: 5 },
    ];
    const { toAdd, conflicts, drifted } = planBackfill(existingEn, entries);
    expect(toAdd).toEqual({ 'a.toAdd': 'Add me' });
    expect(conflicts).toEqual([
      {
        key: 'a.conflict',
        variants: [
          { fallback: 'One', sites: [{ filename: 'X.tsx', line: 2 }] },
          { fallback: 'Two', sites: [{ filename: 'Y.tsx', line: 3 }] },
        ],
      },
    ]);
    expect(drifted).toEqual([
      {
        key: 'a.drifted',
        shipped: 'Old Text',
        variants: [{ fallback: 'New Text', sites: [{ filename: 'X.tsx', line: 5 }] }],
      },
    ]);
  });
});

describe('desktop catalogue split', () => {
  const entry = (key, filename) => ({ key, fallback: key, filename, line: 1 });

  it('treats the desktop modules as desktop-only, and the web door to them as shared', () => {
    expect(isDesktopOnlyPath('src/pages/workspace/WorkspacesPage.tsx')).toBe(true);
    expect(isDesktopOnlyPath('src/features/workspace/ui/TurnTranscript.tsx')).toBe(true);
    expect(isDesktopOnlyPath('src/widgets/desktop-shell/ui/DesktopFrame.tsx')).toBe(true);
    expect(isDesktopOnlyPath('src\\entries\\desktop\\DesktopShell.tsx')).toBe(true);
    expect(isDesktopOnlyPath('src/pages/workspace/desktopEntry.tsx')).toBe(false);
    expect(isDesktopOnlyPath('src/pages/chat/ChatPage.tsx')).toBe(false);
  });

  it('flags a shared key used only by desktop code, a desktop key the web uses, and a key in both', () => {
    const entries = [
      entry('ws.only', 'src/features/workspace/a.tsx'),
      entry('both.sides', 'src/features/workspace/a.tsx'),
      entry('both.sides', 'src/pages/chat/b.tsx'),
      entry('web.only', 'src/pages/chat/b.tsx'),
    ];
    const split = planCatalogueSplit(
      { 'ws.only': 'x', 'web.only': 'y', dup: 'z' },
      { 'both.sides': 'w', dup: 'z' },
      entries,
    );
    expect(split.toDesktop.map((m) => m.key)).toEqual(['ws.only']);
    expect(split.toShared).toEqual([{ key: 'both.sides', sites: [{ filename: 'src/pages/chat/b.tsx', line: 1 }] }]);
    expect(split.duplicated).toEqual(['dup']);
  });

  it('leaves a correctly split catalogue alone', () => {
    const split = planCatalogueSplit({ 'web.only': 'y' }, { 'ws.only': 'x' }, [
      entry('ws.only', 'src/shared/desktop/c.ts'),
      entry('web.only', 'src/app/App.tsx'),
    ]);
    expect(split).toEqual({ toDesktop: [], toShared: [], duplicated: [] });
  });

  it('routes a new key to the desktop catalogue only when every call site is desktop-only', () => {
    const routed = routeNewKeys({ a: 'A', b: 'B', c: 'C' }, [
      entry('a', 'src/pages/workspace/X.tsx'),
      entry('b', 'src/pages/workspace/X.tsx'),
      entry('b', 'src/widgets/sidebar/Y.tsx'),
    ]);
    expect(routed).toEqual({ shared: { b: 'B', c: 'C' }, desktop: { a: 'A' } });
  });
});
