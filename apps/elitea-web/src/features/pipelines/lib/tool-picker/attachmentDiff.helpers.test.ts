import { describe, expect, it } from 'vitest';

import { diffAttachments, getAttachmentKey, isSameToolSet } from './attachmentDiff.helpers';
import type { PickerToolEntry } from './toolPicker.types';

describe('attachmentDiff.helpers', () => {
  it('keys applications by application_id and toolkits by id', () => {
    expect(getAttachmentKey({ id: 9, type: 'github' })).toBe('toolkit:9');
    expect(
      getAttachmentKey({ type: 'application', settings: { application_id: 42 } }),
    ).toBe('application:42');
  });

  it('compares tool sets order-insensitively', () => {
    expect(isSameToolSet(['a', 'b'], ['b', 'a'])).toBe(true);
    expect(isSameToolSet(['a'], ['a', 'b'])).toBe(false);
    expect(isSameToolSet(undefined, [])).toBe(true);
  });

  it('computes added, removed, selectedToolsChanged, and versionChanged diffs', () => {
    const initial: PickerToolEntry[] = [
      { id: 1, type: 'github', settings: { selected_tools: ['a', 'b'] } },
      { id: 2, type: 'jira', settings: { selected_tools: ['search'] } },
      { type: 'application', name: 'Bot', settings: { application_id: 10, application_version_id: 1 } },
    ];
    const current: PickerToolEntry[] = [
      { id: 1, type: 'github', settings: { selected_tools: ['a'] } },
      { id: 3, type: 'slack', settings: { selected_tools: ['post'] } },
      { type: 'application', name: 'Bot', settings: { application_id: 10, application_version_id: 2 } },
    ];

    const diff = diffAttachments(initial, current);
    expect(diff.hasChanges).toBe(true);
    expect(diff.added.map((t) => t.id)).toEqual([3]);
    expect(diff.removed.map((t) => t.id)).toEqual([2]);
    expect(diff.selectedToolsChanged.map((t) => t.id)).toEqual([1]);
    expect(diff.versionChanged).toHaveLength(1);
    expect(diff.versionChanged[0]?.tool.settings?.application_version_id).toBe(2);
    expect(diff.versionChanged[0]?.initialTool.settings?.application_version_id).toBe(1);
  });

  it('reports hasChanges=false when nothing changed', () => {
    const tools: PickerToolEntry[] = [
      { id: 1, type: 'github', settings: { selected_tools: ['b', 'a'] } },
    ];
    const sameOrderSwapped: PickerToolEntry[] = [
      { id: 1, type: 'github', settings: { selected_tools: ['a', 'b'] } },
    ];
    expect(diffAttachments(tools, sameOrderSwapped).hasChanges).toBe(false);
  });
});
