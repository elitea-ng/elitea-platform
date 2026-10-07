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

/** The scroll distance (from rest) past which the greeting has gone and the bar shows the inline "Home". */
export const HOME_TITLE_AFTER = 96;

/** How far the content has to move before the bar gets its material (so nothing slides under a bare bar). */
export const HOME_MATERIAL_AFTER = 1;

/**
 * Where Home's header is: `rest` (transparent, no title), `scrolled` (the bar's material behind
 * the avatar and bell, still no title) and `collapsed` (material and the inline "Home").
 */
export type HomeScroll = 'rest' | 'scrolled' | 'collapsed';

/**
 * Home's header state for a scroll event, measured from the resting offset. On iOS the scroll
 * view adjusts its insets automatically under the transparent bar, so at rest `contentOffset.y`
 * is minus the adjusted inset (status bar + bar, about -106 pt), while the event's `contentInset`
 * is the manual inset (0). `restInset` is that adjusted inset (the header height on iOS, 0 on
 * Android, where the content is padded instead); the larger of it and the reported inset wins.
 */
export function homeScrollState(
  { contentOffset, contentInset }: Pick<NativeScrollEvent, 'contentOffset'> & { contentInset?: Partial<NativeScrollEvent['contentInset']> },
  restInset = 0,
): HomeScroll {
  const moved = contentOffset.y + Math.max(restInset, contentInset?.top ?? 0);
  if (moved > HOME_TITLE_AFTER) return 'collapsed';
  if (moved > HOME_MATERIAL_AFTER) return 'scrolled';
  return 'rest';
}

/**
 * Home's navigation bar (§4.1): transparent over the gradient, the user's avatar leading (opens
 * Settings) and the bell with its badge trailing (a native SF Symbol bar button with the system
 * badge on iOS 26, a 48 dp action with an in-frame badge on Android). The inline title "Home"
 * appears only once the greeting has scrolled away, like a collapsing large title; the bar's
 * material comes as soon as the content moves, so the pill never slides under a bare status bar.
 */
export function homeHeaderOptions(
  design: Pick<Design, 'colors'>,
  {
    collapsed,
    scrolled = collapsed,
    userName,
    avatarUri,
    unseen,
    onBell,
    onAvatar,
  }: { collapsed: boolean; /** The content has moved off rest: the bar gets its material. Defaults to `collapsed`. */ scrolled?: boolean; userName: string | null; avatarUri: string | null; unseen: number; onBell: () => void; onAvatar: () => void },
  on: UiPlatform = uiPlatform,
): NativeStackNavigationOptions {
  return {
    title: 'Home',
    headerTitle: collapsed ? 'Home' : '',
    headerTransparent: true,
    headerShadowVisible: false,
    headerLargeTitle: false,
    headerBlurEffect: (scrolled || collapsed) && on === 'ios' ? 'systemChromeMaterial' : undefined,
    headerStyle: on === 'android' ? { backgroundColor: scrolled || collapsed ? design.colors.background : 'transparent' } : undefined,
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
  /**
   * The account has no usable project in this workspace (none, or all suspended): Home explains
   * that instead of offering cards that would all end at "No project selected" (F1).
   */
  noProject?: boolean;
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
  /** iOS: the inset the system adds under the transparent bar (the header height); 0 on Android. */
  scrollRestInset?: number;
  onScrollState?: (state: HomeScroll) => void;
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
  noProject = false,
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
  scrollRestInset = 0,
  onScrollState,
}: HomeViewProps) {
  const design = useDesign();
  const { colors, accessibilityLayout } = design;
  const margin = useLayoutMargin();
  const onScroll = onScrollState
    ? (event: NativeSyntheticEvent<NativeScrollEvent>) => onScrollState(homeScrollState(event.nativeEvent, scrollRestInset))
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

        {noProject ? null : (
          <>
            <ChipRow>
              <Chip label="Choose an agent" icon="agent" filled menu onPress={onNewChat} />
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
          </>
        )}
      </View>

      {noProject ? (
        <EmptyState
          mascot="bust"
          title="No active project"
          body="Your account has no active project in this workspace. Ask your workspace admin to add you to one, or switch workspace."
        />
      ) : (
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
            // No action of its own (EH-4): the New chat card and the chips above already start one, and a
            // button this low sat under the floating tab bar at rest.
            <EmptyState mascot="bust" title="Ask me anything" body="Your chats show up here. Start one with New chat or your voice." />
          ) : (
            <View style={styles.recents}>
              {recents.slice(0, HOME_RECENTS).map((chat) => (
                <RecentRow key={chat.id} chat={chat} onPress={() => onOpenChat(chat.id)} />
              ))}
            </View>
          )}
        </View>
      )}
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
