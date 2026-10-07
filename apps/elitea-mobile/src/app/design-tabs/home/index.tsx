import { router, Stack, useGlobalSearchParams } from 'expo-router';
import { useHeaderHeight } from 'expo-router/react-navigation';
import { useState } from 'react';

import { useDesign } from '@/design/DesignProvider';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { SAMPLE_CHATS, SAMPLE_ORIGIN, SAMPLE_PROJECT_NAME, SAMPLE_USER_NAME, sampleChatRow } from '@/features/designGallery/fixtures';
import { firstNameOf, HOME_RECENTS, HomeView, homeHeaderOptions, type HomeScroll } from '@/features/home/HomeView';
import { formatAge } from '@/lib/time';
import { GradientBackground, uiPlatform } from '@/ui';

/**
 * Development builds only: Home with sample data (`/design-tabs/home`). It renders the real
 * `HomeView` and `homeHeaderOptions` (D2), so a screenshot of it is the screen the user gets.
 * `?empty=1` shows the no-chats state, `?noProject=1` the no-active-project state; `?brand=` the workspace mark in that brand's accent.
 */
export default function DesignHomePreview() {
  const design = useDesign();
  const params = useGlobalSearchParams<{ brand?: string; empty?: string; offline?: string; noProject?: string }>();
  const [brand] = useState(() => GALLERY_BRANDS[parseGalleryParams(params).brand]);
  const [now] = useState(() => Date.now());
  const [scroll, setScroll] = useState<HomeScroll>('rest');
  const headerHeight = useHeaderHeight();
  const recents = params.empty === '1' ? [] : SAMPLE_CHATS.slice(0, HOME_RECENTS).map((c) => sampleChatRow(c, now)).map((c) => ({ id: c.id, title: c.name, age: formatAge(c.updated_at, now) }));
  return (
    <GradientBackground>
      <Stack.Screen
        options={homeHeaderOptions(design, {
          collapsed: scroll === 'collapsed',
          scrolled: scroll !== 'rest',
          userName: SAMPLE_USER_NAME,
          avatarUri: null,
          unseen: 3,
          onBell: () => router.push('/design-sheet'),
          onAvatar: () => {},
        })}
      />
      <HomeView
        firstName={firstNameOf(SAMPLE_USER_NAME)}
        workspace={{ origin: SAMPLE_ORIGIN, name: brand ? 'Acme Corp AI' : null, logoMark: null, brand }}
        project={SAMPLE_PROJECT_NAME}
        onWorkspace={() => router.push('/design-sheet')}
        offline={params.offline === '1'}
        noProject={params.noProject === '1'}
        recents={recents}
        recentsFailed={false}
        onRetry={() => {}}
        onOpenChat={() => {}}
        onSeeAll={() => router.navigate('/design-tabs/chats')}
        onNewChat={() => {}}
        onDiscover={() => router.navigate('/design-tabs/discover')}
        onVoice={() => {}}
        onAttachPhoto={() => {}}
        onAttachFile={() => {}}
        refreshing={false}
        onRefresh={() => {}}
        topInset={uiPlatform === 'android' ? headerHeight : 0}
        scrollRestInset={uiPlatform === 'ios' ? headerHeight : 0}
        onScrollState={(next) => {
          if (next !== scroll) setScroll(next);
        }}
      />
    </GradientBackground>
  );
}
