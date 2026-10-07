import { Redirect, useGlobalSearchParams } from 'expo-router';
import { useState } from 'react';
import { NativeTabs } from 'expo-router/native-tabs';

import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { nativeTabsProps } from '@/ui';

/**
 * Development builds only: the signed-in shell with sample data (`/design-tabs`), so the native
 * tab bar, large titles, header search and bar buttons can be checked and screenshot without a
 * server. The bar uses the same `nativeTabsProps` as the real tabs. `?brand=zefir|magenta|green|
 * lemon` (read once, when the preview opens) previews a sample brand instead of the connected
 * workspace's; without it the connected workspace's brand applies, as in the real tabs.
 */
export default function DesignTabsLayout() {
  const params = useGlobalSearchParams<{ brand?: string }>();
  const [brand] = useState(() => (params.brand === undefined ? undefined : GALLERY_BRANDS[parseGalleryParams(params).brand]));
  if (!__DEV__) return <Redirect href="/" />;
  if (brand === undefined) return <PreviewTabs />;
  return (
    <DesignProvider brand={brand}>
      <PreviewTabs />
    </DesignProvider>
  );
}

function PreviewTabs() {
  const { colors } = useDesign();
  return (
    <NativeTabs {...nativeTabsProps(colors)}>
      <NativeTabs.Trigger name="home">
        <NativeTabs.Trigger.Label>Home</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'house', selected: 'house.fill' }} md="home" />
      </NativeTabs.Trigger>
      <NativeTabs.Trigger name="chats">
        <NativeTabs.Trigger.Label>Chats</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf={{ default: 'bubble.left.and.bubble.right', selected: 'bubble.left.and.bubble.right.fill' }} md="forum" />
        <NativeTabs.Trigger.Badge>2</NativeTabs.Trigger.Badge>
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
