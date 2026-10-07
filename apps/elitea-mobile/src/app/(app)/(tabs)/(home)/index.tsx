import { router, Stack } from 'expo-router';
import { useHeaderHeight } from 'expo-router/react-navigation';
import { useState } from 'react';

import { flattenConversationPages, useConversations } from '@/api/queries/conversations';
import { useDesign } from '@/design/DesignProvider';
import { firstNameOf, HOME_RECENTS, HomeView, homeHeaderOptions, type HomeScroll } from '@/features/home/HomeView';
import { handOff, type ThenAction } from '@/features/intents/intentHandoff';
import { useNotificationCount } from '@/features/notifications/NotificationBell';
import { useProjectLabel } from '@/features/projects/ProjectChip';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { canDictateOnDevice } from '@/features/speech/dictation';
import { useActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { formatAge } from '@/lib/time';
import { useSelfProfile } from '@/state/selfIdentity';
import { GradientBackground, uiPlatform } from '@/ui';

/** Opens the agent picker; the new conversation then dictates or picks a file, once. */
function newChatThen(then: ThenAction) {
  router.push({ pathname: '/new', params: { intent: handOff({ agent: null, prefill: null, then }) } });
}

/** Home (docs/screens.md §2): the signed-in landing tab. */
export default function HomeScreen() {
  const design = useDesign();
  const profile = useSelfProfile();
  const workspace = useActiveWorkspace();
  const project = useProjectLabel();
  const selection = useProjectSelection();
  const conversations = useConversations(selection.projectId);
  const unseen = useNotificationCount();
  const [scroll, setScroll] = useState<HomeScroll>('rest');
  const [dictation] = useState(canDictateOnDevice);
  const headerHeight = useHeaderHeight();
  const all = flattenConversationPages(conversations.data?.pages);
  const offline = conversations.data?.pages.some((page) => page.offline === true) ?? false;
  const loading = selection.projectId === null || conversations.isLoading;
  return (
    <GradientBackground>
      <Stack.Screen
        options={homeHeaderOptions(design, {
          collapsed: scroll === 'collapsed',
          scrolled: scroll !== 'rest',
          userName: profile?.name ?? null,
          avatarUri: null,
          unseen,
          onBell: () => router.push('/notifications'),
          onAvatar: () => router.navigate('/settings'),
        })}
      />
      <HomeView
        firstName={firstNameOf(profile?.name)}
        workspace={workspace}
        project={project}
        onWorkspace={() => router.push('/projects')}
        offline={offline}
        noProject={selection.noProject}
        recents={loading && !selection.noProject ? null : all.slice(0, HOME_RECENTS).map((c) => ({ id: c.id, title: c.name, age: formatAge(c.updated_at) }))}
        recentsFailed={conversations.error != null}
        onRetry={() => void conversations.refetch()}
        onOpenChat={(id) => router.push({ pathname: '/c/[id]', params: { id: String(id) } })}
        onSeeAll={() => router.navigate('/chats')}
        onNewChat={() => router.push('/new')}
        onDiscover={() => router.navigate('/discover')}
        onVoice={dictation ? () => newChatThen('dictate') : null}
        onAttachPhoto={() => newChatThen('photos')}
        onAttachFile={() => newChatThen('files')}
        refreshing={conversations.isRefetching && !conversations.isFetchingNextPage}
        onRefresh={() => void conversations.refetch()}
        topInset={uiPlatform === 'android' ? headerHeight : 0}
        scrollRestInset={uiPlatform === 'ios' ? headerHeight : 0}
        onScrollState={(next) => {
          if (next !== scroll) setScroll(next);
        }}
      />
    </GradientBackground>
  );
}
