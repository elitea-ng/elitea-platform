import { describe, expect, it } from 'vitest';

import { activateTab, closeTab, defaultInspectorLayout, moveTab, parseInspectorLayout, viewOf } from '../lib/inspectorLayout';
import { mirrorStyles } from '../lib/mirrorStyles';

const LOCAL = ['changes', 'approvals', 'index'] as const;

describe('the inspector layout', () => {
  it('starts with one group holding the page\'s tabs, the first active', () => {
    expect(viewOf(defaultInspectorLayout('local'), [...LOCAL])).toEqual([{ index: 0, tabs: ['changes', 'approvals', 'index'], active: 'changes' }]);
    expect(viewOf(defaultInspectorLayout('chat'), ['conversation'])).toEqual([{ index: 0, tabs: ['conversation'], active: 'conversation' }]);
  });

  it('shows only the tabs the page offers now', () => {
    // No local index on this host: no Index tab.
    expect(viewOf(defaultInspectorLayout('local'), ['changes', 'approvals'])).toEqual([{ index: 0, tabs: ['changes', 'approvals'], active: 'changes' }]);
    expect(viewOf(defaultInspectorLayout('local'), [])).toEqual([]);
  });

  it('splits off a bottom group, moves tabs between the groups and collapses an emptied one', () => {
    const split = moveTab(defaultInspectorLayout('local'), 'index', 1);
    expect(viewOf(split, [...LOCAL])).toEqual([
      { index: 0, tabs: ['changes', 'approvals'], active: 'changes' },
      { index: 1, tabs: ['index'], active: 'index' },
    ]);
    const two = moveTab(split, 'approvals', 1);
    expect(viewOf(two, [...LOCAL])[1]).toEqual({ index: 1, tabs: ['index', 'approvals'], active: 'approvals' });
    // The top group's last tab leaves: the bottom group becomes the only one.
    const lone = moveTab(two, 'changes', 1);
    expect(viewOf(lone, [...LOCAL])).toEqual([{ index: 0, tabs: ['index', 'approvals', 'changes'], active: 'changes' }]);
    expect(lone.groups).toHaveLength(1);
  });

  it('closes a tab until it is reopened, in its place', () => {
    const closed = closeTab(moveTab(defaultInspectorLayout('local'), 'index', 1), 'index');
    // The bottom group has nothing to show: it collapses on screen.
    expect(viewOf(closed, [...LOCAL])).toEqual([{ index: 0, tabs: ['changes', 'approvals'], active: 'changes' }]);
    expect(viewOf(activateTab(closed, 'index'), [...LOCAL])[1]).toEqual({ index: 1, tabs: ['index'], active: 'index' });
  });

  it('falls back to a tab the group really shows', () => {
    const layout = activateTab(defaultInspectorLayout('local'), 'index');
    expect(viewOf(layout, ['changes', 'approvals'])[0]?.active).toBe('changes');
  });

  it('reads back only what it wrote, for its own page', () => {
    const stored = JSON.parse(JSON.stringify(moveTab(defaultInspectorLayout('local'), 'index', 1))) as unknown;
    expect(parseInspectorLayout(stored, 'local')).toEqual(moveTab(defaultInspectorLayout('local'), 'index', 1));
    expect(parseInspectorLayout({ groups: [{ tabs: ['conversation', 'changes', 'changes', 'nope'], active: 'nope' }], closed: ['x'], split: 9, width: 5 }, 'local')).toEqual({
      groups: [{ tabs: ['changes'], active: 'changes' }],
      closed: [],
      split: 0.8,
      width: 260,
    });
    expect(parseInspectorLayout('garbage', 'chat')).toBeNull();
    expect(parseInspectorLayout({ groups: 'x', closed: [], split: 0.5, width: 300 }, 'chat')).toBeNull();
  });
});

describe('mirrorStyles', () => {
  it('copies the source\'s rules and root attributes, then only what was added', () => {
    // Documents with windows of their own, as the main window and a panel window are.
    const [source, target] = [0, 1].map(() => {
      const frame = document.createElement('iframe');
      document.body.append(frame);
      return frame.contentDocument as Document;
    }) as [Document, Document];
    const style = source.createElement('style');
    source.head.append(style);
    style.sheet?.insertRule('.a { color: red; }');
    source.documentElement.setAttribute('data-mui-color-scheme', 'dark');

    const mirror = mirrorStyles(source, target);
    mirror.sync();
    const copies = (): string[] => Array.from(target.querySelectorAll('style[data-mirror]')).flatMap((node) => Array.from((node as HTMLStyleElement).sheet?.cssRules ?? []).map((rule) => rule.cssText));
    expect(copies()).toEqual(['.a { color: red; }']);
    expect(target.documentElement.getAttribute('data-mui-color-scheme')).toBe('dark');

    // A rule inserted through the CSSOM (as emotion does) and a scheme change.
    style.sheet?.insertRule('.b { color: blue; }', 1);
    source.documentElement.setAttribute('data-mui-color-scheme', 'light');
    mirror.sync();
    expect(copies()).toEqual(['.a { color: red; }', '.b { color: blue; }']);
    expect(target.documentElement.getAttribute('data-mui-color-scheme')).toBe('light');

    // A sheet that goes away goes from the target too.
    style.remove();
    mirror.sync();
    expect(copies()).toEqual([]);
    mirror.dispose();
    document.body.innerHTML = '';
  });
});
