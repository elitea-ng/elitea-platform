import { afterEach, describe, expect, it } from 'vitest';

import { installContextMenuGuard } from './contextMenu';

function rightClick(target: Element, init: MouseEventInit = {}): boolean {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, ...init });
  target.dispatchEvent(event);
  return event.defaultPrevented;
}

describe('installContextMenuGuard', () => {
  let uninstall: () => void = () => undefined;
  afterEach(() => {
    uninstall();
    document.body.innerHTML = '';
    window.getSelection()?.removeAllRanges();
  });

  it('suppresses the web page menu on ordinary app UI', () => {
    uninstall = installContextMenuGuard();
    document.body.innerHTML = '<nav><button>Chats</button></nav>';
    expect(rightClick(document.querySelector('button') as Element)).toBe(true);
  });

  it('keeps the system menu in editable fields and over selected text', () => {
    uninstall = installContextMenuGuard();
    document.body.innerHTML = '<textarea></textarea><div contenteditable="true"><span>x</span></div><p>Some answer text</p>';
    expect(rightClick(document.querySelector('textarea') as Element)).toBe(false);
    expect(rightClick(document.querySelector('span') as Element)).toBe(false);
    const paragraph = document.querySelector('p') as Element;
    window.getSelection()?.selectAllChildren(paragraph);
    expect(rightClick(paragraph)).toBe(false);
  });

  it('lets a developer reach the inspector with the Option key', () => {
    uninstall = installContextMenuGuard();
    document.body.innerHTML = '<div>row</div>';
    expect(rightClick(document.querySelector('div') as Element, { altKey: true })).toBe(false);
  });

  it('uninstalls cleanly', () => {
    uninstall = installContextMenuGuard();
    uninstall();
    document.body.innerHTML = '<div>row</div>';
    expect(rightClick(document.querySelector('div') as Element)).toBe(false);
  });
});
