import { describe, expect, it } from 'vitest';

import { actionForAppCommand, actionForShortcut } from '../lib/shellActions';
import { filterEntries, type PaletteEntry } from '../ui/CommandPalette';

const key = (over: Partial<{ key: string; code: string; metaKey: boolean; ctrlKey: boolean; altKey: boolean; shiftKey: boolean }>) => ({
  key: '',
  code: '',
  metaKey: false,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  ...over,
});

describe('actionForShortcut', () => {
  it('maps the shell shortcuts on ⌘ and on Ctrl', () => {
    expect(actionForShortcut(key({ key: 'k', metaKey: true }))).toEqual({ type: 'command_palette' });
    expect(actionForShortcut(key({ key: 'N', ctrlKey: true }))).toEqual({ type: 'new_thread' });
    expect(actionForShortcut(key({ key: 'o', metaKey: true }))).toEqual({ type: 'open_folder' });
    expect(actionForShortcut(key({ key: ',', metaKey: true }))).toEqual({ type: 'settings' });
    expect(actionForShortcut(key({ key: '\\', code: 'Backslash', metaKey: true }))).toEqual({ type: 'toggle_sidebar' });
    expect(actionForShortcut(key({ key: '«', code: 'Backslash', metaKey: true, altKey: true }))).toEqual({ type: 'toggle_changes' });
  });

  it('leaves everything else to the page', () => {
    expect(actionForShortcut(key({ key: 'k' }))).toBeUndefined();
    expect(actionForShortcut(key({ key: 'k', metaKey: true, shiftKey: true }))).toBeUndefined();
    expect(actionForShortcut(key({ key: 'k', metaKey: true, altKey: true }))).toBeUndefined();
    expect(actionForShortcut(key({ key: 'z', metaKey: true }))).toBeUndefined();
  });
});

describe('actionForAppCommand', () => {
  it('maps the host commands, and drops the ones it cannot use', () => {
    expect(actionForAppCommand({ id: 'toggle_changes' })).toEqual({ type: 'toggle_changes' });
    expect(actionForAppCommand({ id: 'back' })).toEqual({ type: 'back' });
    expect(actionForAppCommand({ id: 'workspace_opened', args: { workspace_id: 'w9' } })).toEqual({ type: 'workspaces_changed', workspaceId: 'w9' });
    expect(actionForAppCommand({ id: 'workspace_opened', args: { workspace_id: '' } })).toBeUndefined();
    expect(actionForAppCommand({ id: 'workspace_open_failed', args: { message: 'No access.' } })).toEqual({ type: 'notice', message: 'No access.' });
    expect(actionForAppCommand({ id: 'files_dropped', args: { paths: ['/a.txt'] } })).toEqual({ type: 'files_dropped' });
    expect(actionForAppCommand({ id: 'focus_turn', args: { workspace_id: 'w1', turn_id: 't' } })).toEqual({ type: 'open_workspace', workspaceId: 'w1' });
    expect(actionForAppCommand({ id: 'unknown' } as never)).toBeUndefined();
  });
});

describe('filterEntries', () => {
  const entries: PaletteEntry[] = [
    { id: 'a', label: 'fix the build', group: 'Threads', hint: 'app', action: { type: 'new_thread' } },
    { id: 'b', label: 'Settings', group: 'Actions', action: { type: 'settings' } },
  ];
  it('keeps the entries every word matches, in any field', () => {
    expect(filterEntries(entries, '').map((e) => e.id)).toEqual(['a', 'b']);
    expect(filterEntries(entries, 'BUILD fix').map((e) => e.id)).toEqual(['a']);
    expect(filterEntries(entries, 'threads app').map((e) => e.id)).toEqual(['a']);
    expect(filterEntries(entries, 'nothing')).toEqual([]);
  });
});
