/**
 * The editor's keyboard rule, including the two cases it is easiest to get
 * wrong: a press inside CodeMirror (which already undoes for itself, so acting
 * again would undo TWICE) and Escape outside full screen (which belongs to the
 * drawer, whose close is this editor's SAVE).
 */
import { describe, expect, it } from 'vitest';

import { canvasEditorKeyAction, isEscapeConsumedInside } from './canvasEditorKeys';

const press = (key: string, modifiers: { ctrl?: boolean; meta?: boolean; shift?: boolean } = {}) => ({
  key,
  ctrlKey: modifiers.ctrl ?? false,
  metaKey: modifiers.meta ?? false,
  shiftKey: modifiers.shift ?? false,
});

const OUTSIDE_CODE = { isFullScreen: false, fromCodePane: false };

describe('canvasEditorKeyAction', () => {
  it('undoes on Ctrl-z and on Cmd-z', () => {
    expect(canvasEditorKeyAction(press('z', { ctrl: true }), OUTSIDE_CODE)).toBe('undo');
    expect(canvasEditorKeyAction(press('z', { meta: true }), OUTSIDE_CODE)).toBe('undo');
    // Caps lock on is still the same gesture.
    expect(canvasEditorKeyAction(press('Z', { ctrl: true }), OUTSIDE_CODE)).toBe('undo');
  });

  it('redoes on both spellings the platforms disagree about', () => {
    expect(canvasEditorKeyAction(press('z', { ctrl: true, shift: true }), OUTSIDE_CODE)).toBe('redo');
    expect(canvasEditorKeyAction(press('y', { ctrl: true }), OUTSIDE_CODE)).toBe('redo');
    expect(canvasEditorKeyAction(press('y', { meta: true }), OUTSIDE_CODE)).toBe('redo');
  });

  it('leaves a press inside CodeMirror alone — the pane undoes for itself', () => {
    expect(canvasEditorKeyAction(press('z', { ctrl: true }), { isFullScreen: false, fromCodePane: true })).toBe('ignore');
    expect(canvasEditorKeyAction(press('y', { ctrl: true }), { isFullScreen: false, fromCodePane: true })).toBe('ignore');
  });

  it('ignores an unmodified key, and any other modified one', () => {
    expect(canvasEditorKeyAction(press('z'), OUTSIDE_CODE)).toBe('ignore');
    expect(canvasEditorKeyAction(press('a', { ctrl: true }), OUTSIDE_CODE)).toBe('ignore');
  });

  it('gives Escape to full screen, and to the drawer otherwise', () => {
    expect(canvasEditorKeyAction(press('Escape'), { isFullScreen: true, fromCodePane: false })).toBe('exit-full-screen');
    // Even from inside the code pane: full screen is the editor's own mode.
    expect(canvasEditorKeyAction(press('Escape'), { isFullScreen: true, fromCodePane: true })).toBe('exit-full-screen');
    expect(canvasEditorKeyAction(press('Escape'), OUTSIDE_CODE)).toBe('ignore');
  });
});

describe('isEscapeConsumedInside', () => {
  it('is true when a widget prevented the default (CodeMirror search panel, autocomplete)', () => {
    expect(isEscapeConsumedInside({ defaultPrevented: true, target: document.body })).toBe(true);
  });

  it('is true inside an editing DataGrid cell or row, false elsewhere', () => {
    const row = document.createElement('div');
    row.className = 'MuiDataGrid-row';
    const cell = document.createElement('div');
    cell.className = 'MuiDataGrid-cell MuiDataGrid-cell--editing';
    const input = document.createElement('input');
    cell.append(input);
    row.append(cell);
    expect(isEscapeConsumedInside({ defaultPrevented: false, target: input })).toBe(true);
    cell.className = 'MuiDataGrid-cell';
    expect(isEscapeConsumedInside({ defaultPrevented: false, target: input })).toBe(false);
    row.className = 'MuiDataGrid-row MuiDataGrid-row--editing';
    expect(isEscapeConsumedInside({ defaultPrevented: false, target: input })).toBe(true);
  });

  it('is false for a plain Escape so the drawer still closes', () => {
    expect(isEscapeConsumedInside({ defaultPrevented: false, target: document.createElement('div') })).toBe(false);
    expect(isEscapeConsumedInside({ defaultPrevented: false, target: null })).toBe(false);
  });
});
