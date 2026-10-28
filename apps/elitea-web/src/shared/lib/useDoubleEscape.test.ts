import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DOUBLE_ESCAPE_MS, useDoubleEscape } from './useDoubleEscape';

const escape = (init: KeyboardEventInit = {}): KeyboardEvent => {
  const event = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true, ...init });
  document.body.dispatchEvent(event);
  return event;
};

describe('useDoubleEscape', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  it('calls back on two presses within the window, not on one', () => {
    const onDouble = vi.fn();
    renderHook(() => useDoubleEscape(true, onDouble));
    escape();
    expect(onDouble).not.toHaveBeenCalled();
    vi.advanceTimersByTime(DOUBLE_ESCAPE_MS - 100);
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
    // The pair is used: one more press starts a new one.
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
  });

  it('does not pair presses further apart than the window', () => {
    const onDouble = vi.fn();
    renderHook(() => useDoubleEscape(true, onDouble));
    escape();
    vi.advanceTimersByTime(DOUBLE_ESCAPE_MS + 1);
    escape();
    expect(onDouble).not.toHaveBeenCalled();
    vi.advanceTimersByTime(100);
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
  });

  it('listens only while active', () => {
    const onDouble = vi.fn();
    const { rerender } = renderHook(({ active }) => useDoubleEscape(active, onDouble), { initialProps: { active: false } });
    escape();
    escape();
    expect(onDouble).not.toHaveBeenCalled();
    rerender({ active: true });
    escape();
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
  });

  it('leaves Escape to an open menu or dialog', () => {
    const onDouble = vi.fn();
    renderHook(() => useDoubleEscape(true, onDouble));
    const menu = document.createElement('ul');
    menu.setAttribute('role', 'menu');
    document.body.append(menu);
    escape();
    escape();
    expect(onDouble).not.toHaveBeenCalled();
    const dialog = document.createElement('div');
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    menu.replaceWith(dialog);
    escape();
    escape();
    expect(onDouble).not.toHaveBeenCalled();
  });

  it('does not count a press something else handled as the first of a pair', () => {
    const onDouble = vi.fn();
    renderHook(() => useDoubleEscape(true, onDouble));
    const handled = (event: KeyboardEvent): void => event.preventDefault();
    document.body.addEventListener('keydown', handled, { once: true });
    escape();
    escape();
    expect(onDouble).not.toHaveBeenCalled();
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
  });

  it('ignores other keys between and a held key', () => {
    const onDouble = vi.fn();
    renderHook(() => useDoubleEscape(true, onDouble));
    escape();
    escape({ repeat: true });
    expect(onDouble).not.toHaveBeenCalled();
    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
    escape();
    expect(onDouble).toHaveBeenCalledOnce();
  });
});
