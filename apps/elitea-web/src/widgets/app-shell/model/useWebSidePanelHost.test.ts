import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import { useWebSidePanelHost } from './useWebSidePanelHost';

// jsdom has no matchMedia: MUI's useMediaQuery answers false, so this is a
// window neither narrow nor wide — closed until the viewer opens it.
describe('useWebSidePanelHost', () => {
  afterEach(() => createStorage('local').remove('sidePanel.open'));

  it('offers nothing and shows nothing before a page registers a tab', () => {
    const { result } = renderHook(() => useWebSidePanelHost());
    expect(result.current.tabs).toEqual([]);
    expect(result.current.active).toBeNull();
    expect(result.current.shown).toBe(false);
  });

  it('opens on the tab a page asks for and targets only that tab, once the body is mounted', () => {
    const { result } = renderHook(() => useWebSidePanelHost());
    act(() => {
      result.current.slot.register({ id: 'conversation', title: 'Conversation', subject: 'Chat' });
      result.current.slot.register({ id: 'filters', title: 'Filters', subject: 'Agents' });
    });
    expect(result.current.tabs.map((tab) => tab.id)).toEqual(['conversation', 'filters']);
    expect(result.current.active).toBe('conversation');
    expect(result.current.shown).toBe(false);

    act(() => result.current.slot.show('filters'));
    expect(result.current.active).toBe('filters');
    expect(result.current.shown).toBe(true);
    expect(result.current.slot.targets.size).toBe(0);

    const body = document.createElement('div');
    act(() => result.current.setBody(body));
    expect([...result.current.slot.targets.keys()]).toEqual(['filters']);
    expect(result.current.slot.targets.get('filters')?.node).toBe(body);
  });

  it('remembers open or closed for the next visit', () => {
    const first = renderHook(() => useWebSidePanelHost());
    act(() => first.result.current.setOpen(true));
    expect(createStorage('local').get('sidePanel.open')).toBe('true');
    first.unmount();

    const { result } = renderHook(() => useWebSidePanelHost());
    act(() => {
      result.current.slot.register({ id: 'filters', title: 'Filters', subject: 'Agents' });
    });
    expect(result.current.shown).toBe(true);
  });

  it('drops a tab when its page unregisters it', () => {
    const { result } = renderHook(() => useWebSidePanelHost());
    let unregister = (): void => undefined;
    act(() => {
      unregister = result.current.slot.register({ id: 'filters', title: 'Filters', subject: 'Agents' });
    });
    act(() => unregister());
    expect(result.current.tabs).toEqual([]);
  });
});
