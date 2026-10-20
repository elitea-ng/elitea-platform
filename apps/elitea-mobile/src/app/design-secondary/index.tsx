import { router, Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import { OFFLINE_EMPTY } from '@/features/chats/offlineEmpty';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { DiscoverView, type DiscoverLayout } from '@/features/discover/DiscoverView';
import { SAMPLE_ORIGIN, SAMPLE_PROJECT_ID } from '@/features/designGallery/fixtures';
import { SAMPLE_MEMORIES, SAMPLE_PUBLIC_AGENTS, sampleDevices, sampleInbox } from '@/features/designGallery/secondaryFixtures';
import { filterOptions, type NotificationFilter } from '@/features/notifications/notificationModel';
import { NotificationsView } from '@/features/notifications/NotificationsView';
import { AboutView } from '@/features/settings/AboutView';
import { DevicesView } from '@/features/settings/DevicesView';
import { MemoryView } from '@/features/settings/MemoryView';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import { formatAge } from '@/lib/time';
import { Button, headerActionOptions, largeTitleOptions, searchBarOptions } from '@/ui';

const SCREENS = [
  'discover',
  'discover-list',
  'discover-empty',
  'discover-error',
  'discover-unavailable',
  'notifications',
  'notifications-empty',
  'memory',
  'memory-empty',
  'devices',
  'about',
  'offline',
  'error',
] as const;
type Screen = (typeof SCREENS)[number];

/** Development builds only; see the layout. `?screen=` picks the screen. */
export default function DesignSecondaryPreview() {
  const params = useLocalSearchParams<{ screen?: string }>();
  const screen: Screen = SCREENS.includes(params.screen as Screen) ? (params.screen as Screen) : 'discover';
  return <Preview key={screen} screen={screen} />;
}

function Preview({ screen }: { screen: Screen }) {
  const design = useDesign();
  const [now] = useState(() => Date.now());
  const [layout, setLayout] = useState<DiscoverLayout>(screen === 'discover-list' ? 'list' : 'grid');
  const [filter, setFilter] = useState<NotificationFilter>('all');
  const noop = () => {};

  if (screen.startsWith('discover') && screen !== 'discover-unavailable') {
    const status = screen === 'discover-error' ? 'error' : 'ready';
    return (
      <>
        <Stack.Screen
          options={{
            title: 'Discover',
            ...largeTitleOptions(design),
            ...searchBarOptions(design, { placeholder: 'Search public agents', onChange: noop }),
            ...headerActionOptions(
              [
                layout === 'grid'
                  ? { id: 'layout', icon: 'list', label: 'Show as list', onPress: () => setLayout('list') }
                  : { id: 'layout', icon: 'grid', label: 'Show as grid', onPress: () => setLayout('grid') },
              ],
              design,
            ),
          }}
        />
        <DiscoverView
          agents={screen === 'discover-empty' ? [] : SAMPLE_PUBLIC_AGENTS}
          projectId={SAMPLE_PROJECT_ID}
          layout={layout}
          status={status}
          query=""
          onOpen={() => router.push('/design-secondary/agent')}
          onRetry={noop}
          header={<WorkspacePillView origin={SAMPLE_ORIGIN} image={null} />}
        />
      </>
    );
  }

  switch (screen) {
    case 'discover-unavailable':
      return (
        <>
          <Stack.Screen options={{ title: 'Discover', ...largeTitleOptions(design) }} />
          <StatusScreen mascot="bust" title="Discover isn't available" body="This workspace does not offer public agents to mobile yet." />
        </>
      );
    case 'notifications':
    case 'notifications-empty': {
      const rows = screen === 'notifications' ? sampleInbox(now) : [];
      const filters = filterOptions(rows);
      return (
        <>
          <Stack.Screen
            options={{
              title: 'Notifications',
              ...headerActionOptions(
                rows.some((r) => !r.is_seen) ? [{ id: 'mark-all', icon: 'markAllRead', label: 'Mark all as read', onPress: noop }] : [],
                design,
              ),
            }}
          />
          <NotificationsView
            rows={filter === 'all' ? rows : rows.filter((r) => r.event_type === filter)}
            now={now}
            status="ready"
            filters={filters}
            filter={filter}
            onFilter={setFilter}
            onOpen={noop}
            onDismiss={async () => false}
            onRetry={noop}
          />
        </>
      );
    }
    case 'memory':
    case 'memory-empty':
      return (
        <>
          <Stack.Screen options={{ title: 'Memory' }} />
          <MemoryView
            status={{ kind: 'ready' }}
            rows={screen === 'memory' ? SAMPLE_MEMORIES : []}
            banner={null}
            selectable
            onDelete={async () => false}
            onClearAll={noop}
            onRetry={noop}
          />
        </>
      );
    case 'devices':
      return (
        <>
          <Stack.Screen options={{ title: 'Signed-in devices' }} />
          <DevicesView
            status={{ kind: 'ready' }}
            devices={sampleDevices(now)}
            host="acme.elitea.ai"
            age={(iso) => formatAge(iso, now)}
            banner={null}
            onRevoke={async () => false}
            onRetry={noop}
          />
        </>
      );
    case 'about':
      return (
        <>
          <Stack.Screen options={{ title: 'About' }} />
          <AboutView version="0.1.0" onPrivacy={noop} onSupport={noop} onDesignGallery={() => router.push('/design-gallery')} />
        </>
      );
    case 'offline':
      return (
        <>
          <Stack.Screen options={{ title: 'Chats' }} />
          <StatusScreen mascot="bust" title={OFFLINE_EMPTY.title} body={OFFLINE_EMPTY.body}>
            <Button title="Try again" size="small" variant="tonal" onPress={noop} />
          </StatusScreen>
        </>
      );
    case 'error':
      return (
        <>
          <Stack.Screen options={{ title: 'Chats' }} />
          <StatusScreen title="Couldn't load conversations" body="Check your connection and try again.">
            <Button title="Try again" size="small" variant="tonal" onPress={noop} />
          </StatusScreen>
        </>
      );
  }
}
