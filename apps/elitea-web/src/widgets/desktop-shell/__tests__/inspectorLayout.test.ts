import { describe, expect, it, vi } from 'vitest';

import { activateTab, byTabOrder, closeTab, defaultInspectorLayout, moveTab, parseInspectorLayout, reopenIfAllClosed, splitTab, viewOf } from '../lib/inspectorLayout';
import { mirrorStyles, watchStyles } from '../lib/mirrorStyles';
import { createDesktopLayoutStore, selectInspectorShown } from '../model/desktopLayout.store';

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

  it('splits a tab off BELOW the one group on screen, even when that is the stored bottom group', () => {
    // The top group holds only a closed tab: the bottom group is the one on screen.
    const layout = closeTab(moveTab(moveTab(defaultInspectorLayout('local'), 'index', 1), 'approvals', 1), 'changes');
    expect(viewOf(layout, [...LOCAL])).toEqual([{ index: 1, tabs: ['index', 'approvals'], active: 'approvals' }]);
    const split = splitTab(layout, 'index', 1);
    expect(viewOf(split, [...LOCAL])).toEqual([
      { index: 0, tabs: ['approvals'], active: 'approvals' },
      { index: 1, tabs: ['index'], active: 'index' },
    ]);
    // The closed tab keeps its place (the top group) and stays closed.
    expect(split.groups[0]?.tabs).toEqual(['approvals', 'changes']);
    expect(split.closed).toEqual(['changes']);
    // The common case: one stored group.
    expect(viewOf(splitTab(defaultInspectorLayout('local'), 'changes', 0), [...LOCAL])).toEqual([
      { index: 0, tabs: ['approvals', 'index'], active: 'approvals' },
      { index: 1, tabs: ['changes'], active: 'changes' },
    ]);
  });

  it('reopens the page\'s tabs only when every one it offers is closed', () => {
    const some = closeTab(defaultInspectorLayout('local'), 'changes');
    expect(reopenIfAllClosed(some, [...LOCAL])).toBe(some);
    const all = LOCAL.reduce(closeTab, defaultInspectorLayout('local'));
    expect(viewOf(all, [...LOCAL])).toEqual([]);
    expect(viewOf(reopenIfAllClosed(all, ['changes', 'approvals']), ['changes', 'approvals'])[0]?.tabs).toEqual(['changes', 'approvals']);
    // A tab this host does not offer stays as the person left it.
    expect(reopenIfAllClosed(all, ['changes', 'approvals']).closed).toEqual(['index']);
  });

  it('orders tabs by their fixed order, not by registration', () => {
    expect(byTabOrder([{ id: 'index' }, { id: 'changes' }, { id: 'approvals' }] as const).map((tab) => tab.id)).toEqual(['changes', 'approvals', 'index']);
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

describe('the inspector in the layout store', () => {
  const offered = (store: ReturnType<typeof createDesktopLayoutStore>): void => store.getState().setInspectorOffer({ page: 'local', tabs: [...LOCAL] });

  it('brings every closed tab back when the panel is shown or toggled', () => {
    const store = createDesktopLayoutStore();
    offered(store);
    LOCAL.forEach((tab) => store.getState().closeInspectorTab(tab));
    expect(selectInspectorShown(store.getState())).toBe(false);
    // ⌘⌥\ with nothing on screen shows the panel, its tabs reopened (not a hide that needs a second press).
    store.getState().toggleInspector();
    expect(store.getState().changesOpen).toBe(true);
    expect(selectInspectorShown(store.getState())).toBe(true);
    store.getState().toggleInspector();
    expect(store.getState().changesOpen).toBe(false);

    LOCAL.forEach((tab) => store.getState().closeInspectorTab(tab));
    store.getState().setChangesOpen(true);
    expect(viewOf(store.getState().inspector.local, [...LOCAL])[0]?.tabs).toEqual([...LOCAL]);
  });

  it('toggles plainly on a page without tabs', () => {
    const store = createDesktopLayoutStore();
    store.getState().toggleInspector();
    expect(store.getState().changesOpen).toBe(false);
    store.getState().toggleInspector();
    expect(store.getState().changesOpen).toBe(true);
  });

  it('drops "maximized" when the panel hides, its tabs all close or go, or the page changes', () => {
    const store = createDesktopLayoutStore();
    const maximize = (): void => {
      offered(store);
      store.getState().setChangesOpen(true);
      store.getState().setInspectorMaximized(true);
      expect(store.getState().inspectorMaximized).toBe(true);
    };
    maximize();
    store.getState().setChangesOpen(false);
    expect(store.getState().inspectorMaximized).toBe(false);

    maximize();
    LOCAL.forEach((tab) => store.getState().closeInspectorTab(tab));
    expect(store.getState().inspectorMaximized).toBe(false);

    maximize();
    store.getState().setInspectorOffer({ page: null, tabs: [] });
    expect(store.getState().inspectorMaximized).toBe(false);

    maximize();
    store.getState().setInspectorOffer({ page: 'chat', tabs: ['conversation'] });
    expect(store.getState().inspectorMaximized).toBe(false);

    // A tab moving between groups keeps it.
    maximize();
    store.getState().moveInspectorTab('index', 1);
    expect(store.getState().inspectorMaximized).toBe(true);
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

    // A sheet copied afresh (it shrank) goes back in its place, before the next sheet's copy: the cascade keeps its order.
    const later = source.createElement('style');
    source.head.append(later);
    later.sheet?.insertRule('.z { color: green; }');
    mirror.sync();
    style.sheet?.deleteRule(1);
    mirror.sync();
    expect(copies()).toEqual(['.a { color: red; }', '.z { color: green; }']);
    later.remove();

    // A sheet that goes away goes from the target too.
    style.remove();
    mirror.sync();
    expect(copies()).toEqual([]);
    mirror.dispose();
    document.body.innerHTML = '';
  });
});

describe('watchStyles', () => {
  it('reports a CSSOM insert, a head change and a root attribute change, once per microtask, until disposed', async () => {
    const frame = document.createElement('iframe');
    document.body.append(frame);
    const source = frame.contentDocument as Document;
    const style = source.createElement('style');
    source.head.append(style);
    const onChange = vi.fn();
    const stop = watchStyles(source, onChange);
    const settle = (): Promise<void> => new Promise((resolve) => window.setTimeout(resolve, 0));

    style.sheet?.insertRule('.a { color: red; }');
    style.sheet?.insertRule('.b { color: red; }');
    await settle();
    expect(onChange).toHaveBeenCalledTimes(1);
    source.head.append(source.createElement('style'));
    await settle();
    source.documentElement.setAttribute('data-mui-color-scheme', 'dark');
    await settle();
    expect(onChange).toHaveBeenCalledTimes(3);

    stop();
    style.sheet?.insertRule('.c { color: red; }');
    source.documentElement.setAttribute('data-mui-color-scheme', 'light');
    await settle();
    expect(onChange).toHaveBeenCalledTimes(3);
    document.body.innerHTML = '';
  });
});
