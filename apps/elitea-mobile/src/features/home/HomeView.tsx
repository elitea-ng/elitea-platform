import type { NativeStackNavigationOptions } from 'expo-router';
import { ActivityIndicator, Pressable, RefreshControl, ScrollView, StyleSheet, View, type NativeScrollEvent, type NativeSyntheticEvent } from 'react-native';

import { OfflineNotice } from '@/components/OfflineNotice';
import type { Design } from '@/design/resolve';
import { useDesign } from '@/design/DesignProvider';
import { notificationBellAction } from '@/features/notifications/NotificationBell';
import type { ActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import {
  ActionCard,
  ArrowButton,
  Avatar,
  Chip,
  ChipRow,
  EmptyState,
  GlassSurface,
  headerActionOptions,
  hitTarget,
  pressedOverlay,
  ripple,
  Text,
  uiPlatform,
  useLayoutMargin,
  type UiPlatform,
} from '@/ui';

export interface RecentChat {
  id: number;
  title: string;
  /** "9:41", "Yesterday", "Sep 28". */
  age: string;
}

/** The first word of a profile name, for the greeting ("Alex Smith" → "Alex"). */
export function firstNameOf(name: string | null | undefined): string | null {
  const first = (name ?? '').trim().split(/\s+/)[0] ?? '';
  return first === '' ? null : first;
}

/** How many recent chats Home lists (docs/screens.md §2: "max 5"). */
export const HOME_RECENTS = 5;

/** The scroll offset past which the greeting has gone and the bar shows the inline "Home". */
export const HOME_TITLE_AFTER = 96;

/**
 * Home's navigation bar (§4.1): transparent over the gradient, the user's avatar leading (opens
 * Settings) and the bell with its badge trailing (a native SF Symbol bar button with the system
 * badge on iOS 26, a 48 dp action with an in-frame badge on Android). The inline title "Home"
 * appears only once the greeting has scrolled away, like a collapsing large title.
 */
export function homeHeaderOptions(
  design: Pick<Design, 'colors'>,
  {
    collapsed,
    userName,
    avatarUri,
    unseen,
    onBell,
    onAvatar,
  }: { collapsed: boolean; userName: string | null; avatarUri: string | null; unseen: number; onBell: () => void; onAvatar: () => void },
  on: UiPlatform = uiPlatform,
): NativeStackNavigationOptions {
  return {
    title: 'Home',
    headerTitle: collapsed ? 'Home' : '',
    headerTransparent: true,
    headerShadowVisible: false,
    headerLargeTitle: false,
    headerBlurEffect: collapsed && on === 'ios' ? 'systemChromeMaterial' : undefined,
    headerStyle: on === 'android' ? { backgroundColor: collapsed ? design.colors.background : 'transparent' } : undefined,
    headerTitleStyle: { color: design.colors.textPrimary },
    headerLeft: () => <HeaderAvatar name={userName} uri={avatarUri} onPress={onAvatar} />,
    ...headerActionOptions([{ ...notificationBellAction(unseen), onPress: onBell }], design, on),
  };
}

function HeaderAvatar({ name, uri, onPress }: { name: string | null; uri: string | null; onPress: () => void }) {
  const design = useDesign();
  return (
    <Pressable
      testID="home-avatar"
      accessibilityRole="button"
      accessibilityLabel={name ? `${name}. Settings` : 'Settings'}
      onPress={onPress}
      android_ripple={ripple(design, true)}
      hitSlop={4}
      style={({ pressed }) => [styles.headerAvatar, { minHeight: hitTarget(), minWidth: hitTarget() }, pressed && uiPlatform === 'ios' ? styles.pressed : null]}>
      <Avatar name={name ?? '?'} imageUri={uri} size={36} decorative />
    </Pressable>
  );
}

export interface HomeViewProps {
  firstName: string | null;
  workspace: ActiveWorkspace | null;
  /** The selected project's name ("My project", "Marketing"); shown in the workspace pill. */
  project: string | null;
  onWorkspace: () => void;
  offline: boolean;
  /** Null while loading. */
  recents: readonly RecentChat[] | null;
  recentsFailed: boolean;
  onRetry: () => void;
  onOpenChat: (id: number) => void;
  onSeeAll: () => void;
  onNewChat: () => void;
  onDiscover: () => void;
  /** Dictation into a new chat; null where the device cannot dictate on-device. */
  onVoice: (() => void) | null;
  onAttachPhoto: () => void;
  onAttachFile: () => void;
  refreshing: boolean;
  onRefresh: () => void;
  /** Android: the transparent top app bar's height (iOS adjusts the scroll insets itself). */
  topInset?: number;
  onScrolledPast?: (past: boolean) => void;
}

/**
 * Home (docs/screens.md §2, the reference board's assistant home): the workspace pill (host
 * always visible, the project by name), the greeting with the user's first name in the accent,
 * a chip row (chat, mic, photo, file), four action cards (New chat, Discover, Dictate, Attach)
 * and the five most recent chats. On the pastel gradient; at accessibility sizes the cards become
 * one column and the chips wrap. No composer: the cards and chips are the entry points.
 */
export function HomeView({
  firstName,
  workspace,
  project,
  onWorkspace,
  offline,
  recents,
  recentsFailed,
  onRetry,
  onOpenChat,
  onSeeAll,
  onNewChat,
  onDiscover,
  onVoice,
  onAttachPhoto,
  onAttachFile,
  refreshing,
  onRefresh,
  topInset = 0,
  onScrolledPast,
}: HomeViewProps) {
  const design = useDesign();
  const { colors, accessibilityLayout } = design;
  const margin = useLayoutMargin();
  const onScroll = onScrolledPast
    ? (event: NativeSyntheticEvent<NativeScrollEvent>) => {
        const { contentOffset, contentInset } = event.nativeEvent;
        onScrolledPast(contentOffset.y + (contentInset?.top ?? 0) > HOME_TITLE_AFTER);
      }
    : undefined;
  const cards = [
    { key: 'new', title: 'New chat', icon: 'newChat' as const, hint: 'Pick an agent and start a chat', onPress: onNewChat },
    { key: 'discover', title: 'Discover agents', icon: 'discover' as const, hint: "Opens your workspace's agent catalogue", onPress: onDiscover },
    onVoice
      ? { key: 'voice', title: 'Dictate a message', icon: 'mic' as const, hint: 'Pick an agent, then speak; the words are typed on this device', onPress: onVoice }
      : null,
    { key: 'attach', title: 'Ask about a file', icon: 'attach' as const, hint: 'Pick an agent, then a file to send with your question', onPress: onAttachFile },
  ].filter((card) => card !== null);
  const rows: (typeof cards)[] = [];
  const perRow = accessibilityLayout ? 1 : 2;
  for (let i = 0; i < cards.length; i += perRow) rows.push(cards.slice(i, i + perRow));

  return (
    <ScrollView
      testID="home"
      contentInsetAdjustmentBehavior="automatic"
      onScroll={onScroll}
      scrollEventThrottle={32}
      style={styles.flex}
      contentContainerStyle={[styles.content, { paddingTop: topInset + 4 }]}
      refreshControl={<RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={colors.accent} progressViewOffset={topInset} />}>
      {workspace ? (
        <WorkspacePillView
          origin={workspace.origin}
          image={workspace.logoMark}
          brand={workspace.brand}
          name={workspace.name}
          project={project}
          onPress={onWorkspace}
        />
      ) : null}
      <OfflineNotice visible={offline} />

      <View style={[styles.section, { paddingHorizontal: margin }]}>
        {/* One node for VoiceOver; the accent is not the only signal (the words are the same). */}
        <Text variant="display" accessibilityRole="header" testID="home-greeting">
          {firstName ? (
            <>
              Hello, <Text variant="display" tone="accent">{firstName}</Text>
            </>
          ) : (
            'Hello'
          )}
          {'\n'}
          <Text variant="display">What can I help with?</Text>
        </Text>

        <ChipRow>
          <Chip label="Choose an agent" icon="agent" selected menu onPress={onNewChat} />
          {onVoice ? <Chip label="Dictate" icon="mic" iconOnly onPress={onVoice} /> : null}
          <Chip label="Ask about a photo" icon="image" iconOnly onPress={onAttachPhoto} />
          <Chip label="Ask about a file" icon="attach" iconOnly onPress={onAttachFile} />
        </ChipRow>

        <View style={styles.grid}>
          {rows.map((row) => (
            <View key={row.map((c) => c.key).join('-')} style={styles.gridRow}>
              {row.map((card) => (
                <ActionCard key={card.key} testID={`home-card-${card.key}`} title={card.title} icon={card.icon} accessibilityHint={card.hint} onPress={card.onPress} />
              ))}
              {row.length < perRow ? <View style={styles.flex} /> : null}
            </View>
          ))}
        </View>
      </View>

      <View style={[styles.section, { paddingHorizontal: margin }]}>
        <View style={styles.recentsHeader}>
          <Text variant="title3" heading>
            Recent chats
          </Text>
          {recents && recents.length > 0 ? (
            <Pressable
              testID="home-see-all"
              accessibilityRole="button"
              accessibilityLabel="See all chats"
              onPress={onSeeAll}
              hitSlop={8}
              style={({ pressed }) => [{ minHeight: hitTarget(), justifyContent: 'center' }, pressed ? styles.pressed : null]}>
              <Text variant="subheadline" tone="accent" emphasized>
                See all
              </Text>
            </Pressable>
          ) : null}
        </View>
        {recents === null ? (
          <ActivityIndicator color={colors.accent} style={styles.loading} />
        ) : recentsFailed && recents.length === 0 ? (
          <EmptyState mascot="none" title="Couldn't load your chats" body="Check your connection and try again." action={{ title: 'Try again', onPress: onRetry }} />
        ) : recents.length === 0 ? (
          <EmptyState mascot="bust" title="Ask me anything" body="Start a chat or try your voice." action={{ title: 'New chat', onPress: onNewChat }} />
        ) : (
          <View style={styles.recents}>
            {recents.slice(0, HOME_RECENTS).map((chat) => (
              <RecentRow key={chat.id} chat={chat} onPress={() => onOpenChat(chat.id)} />
            ))}
          </View>
        )}
      </View>
    </ScrollView>
  );
}

/** A "Recent chats" row (§4.4): glass, radius 16, a chat tile, the title and age, the arrow (visual). */
function RecentRow({ chat, onPress }: { chat: RecentChat; onPress: () => void }) {
  const design = useDesign();
  const title = chat.title || 'Untitled conversation';
  return (
    <Pressable
      testID={`home-recent-${chat.id}`}
      accessibilityRole="button"
      accessibilityLabel={`${title}, ${chat.age}`}
      accessibilityHint="Opens the chat"
      onPress={onPress}
      android_ripple={ripple(design)}
      style={({ pressed }) => (pressed && uiPlatform === 'ios' ? styles.pressed : null)}>
      <GlassSurface radius={design.radius.listRow} style={[styles.recent, { minHeight: Math.max(64, hitTarget()) }]}>
        <View
          accessible={false}
          importantForAccessibility="no-hide-descendants"
          style={[styles.recentTile, { backgroundColor: design.colors.accentSoft }]}>
          <Text variant="headline" color={design.colors.accentSoftOn} maxFontSizeMultiplier={1}>
            {Array.from(title)[0]?.toUpperCase() ?? '·'}
          </Text>
        </View>
        <View style={styles.recentText}>
          <Text variant="headline" numberOfLines={design.accessibilityLayout ? 3 : 1}>
            {title}
          </Text>
          <Text variant="footnote" tone="secondary">
            {chat.age}
          </Text>
        </View>
        <ArrowButton size={36} />
      </GlassSurface>
    </Pressable>
  );
}

/** The pressed overlay of a row (exported for the kit's target tests). */
export const homePressedOverlay = pressedOverlay;

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { paddingBottom: 32, gap: 20 },
  section: { gap: 16 },
  grid: { gap: 12 },
  gridRow: { flexDirection: 'row', gap: 12 },
  recentsHeader: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', gap: 12 },
  recents: { gap: 10 },
  recent: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 14, paddingVertical: 12 },
  recentTile: { width: 36, height: 36, borderRadius: 18, alignItems: 'center', justifyContent: 'center' },
  recentText: { flex: 1, gap: 2 },
  headerAvatar: { alignItems: 'center', justifyContent: 'center' },
  loading: { padding: 24 },
  pressed: { opacity: 0.7 },
});
