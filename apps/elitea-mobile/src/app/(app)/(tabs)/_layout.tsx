import { NativeTabs } from 'expo-router/native-tabs';

import { usePalette } from '@/constants/use-palette';

/**
 * The signed-in tab bar: Chats, Discover and Settings (docs/screens.md §0).
 * The set of tabs never changes: expo-router keys the native tab view by the
 * visible trigger names, so hiding Discover until the workspace's discovery
 * document arrives (after launch) would remount the bar and every tab's
 * content. Discover therefore always has its tab, and its screen says when
 * the workspace (below client contract 1.3) does not offer it.
 * A thread, an agent page and the sheets are pushed on the stack above the
 * tabs, so the tab bar is not shown there.
 */
export default function TabsLayout() {
  const palette = usePalette();
  return (
    <NativeTabs tintColor={palette.accent}>
      <NativeTabs.Trigger name="(chats)">
        <NativeTabs.Trigger.Label>Chats</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'bubble.left.and.bubble.right', selected: 'bubble.left.and.bubble.right.fill' }} md="forum" />
      </NativeTabs.Trigger>
      <NativeTabs.Trigger name="discover">
        <NativeTabs.Trigger.Label>Discover</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'sparkle.magnifyingglass', selected: 'sparkle.magnifyingglass' }} md="explore" />
      </NativeTabs.Trigger>
      <NativeTabs.Trigger name="settings">
        <NativeTabs.Trigger.Label>Settings</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'gearshape', selected: 'gearshape.fill' }} md="settings" />
      </NativeTabs.Trigger>
    </NativeTabs>
  );
}
