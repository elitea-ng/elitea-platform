import { NativeTabs } from 'expo-router/native-tabs';

import { useDesign } from '@/design/DesignProvider';
import { nativeTabsProps } from '@/ui';

/**
 * The signed-in tab bar: Home, Chats, Discover and Settings (docs/screens.md §2, design-system
 * §4.12). Home is first, so it is where a signed-in launch lands.
 * The set of tabs never changes: expo-router keys the native tab view by the
 * visible trigger names, so hiding Discover until the workspace's discovery
 * document arrives (after launch) would remount the bar and every tab's
 * content. Discover therefore always has its tab, and its screen says when
 * the workspace (below client contract 1.3) does not offer it.
 * A thread, an agent page and the sheets are pushed on the stack above the
 * tabs, so the tab bar is not shown there.
 * The bar's look (Liquid Glass on iOS 26, M3 navigation bar on Android) is `nativeTabsProps`.
 */
export default function TabsLayout() {
  const { colors } = useDesign();
  return (
    <NativeTabs {...nativeTabsProps(colors)}>
      <NativeTabs.Trigger name="(home)">
        <NativeTabs.Trigger.Label>Home</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'house', selected: 'house.fill' }} md="home" />
      </NativeTabs.Trigger>
      <NativeTabs.Trigger name="chats">
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
