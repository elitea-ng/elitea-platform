import type { ReactElement } from 'react';
import { ActivityIndicator, FlatList, ScrollView, StyleSheet, Switch, View, type RefreshControlProps } from 'react-native';

import type { AgentMemoryEntry } from '@/api/queries/agentMemory';
import { useDesign } from '@/design/DesignProvider';
import { formatShortDate, parseServerTime } from '@/lib/time';
import { Button, EmptyState, Icon, ListRow, ListSection, Text, sectionTitle, uiPlatform, useLayoutMargin } from '@/ui';

export interface AgentMemoryViewProps {
  agentName: string;
  status: { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string } | { kind: 'unavailable'; message: string };
  rows: readonly AgentMemoryEntry[];
  total: number;
  /** User id → name, from the list's `people`. */
  people: Readonly<Record<number, string>>;
  selfUserId: number | null;
  /** Whether a row offers Delete (a note about the caller, or any with `can_delete`). */
  canDelete: (entry: AgentMemoryEntry) => boolean;
  /** The entry whose delete is running. */
  deleting: number | null;
  optedOut: boolean;
  optOutBusy: boolean;
  /** A failed refresh, delete or opt-out, shown above the rows that are still there. */
  banner: string | null;
  /** The workspace allows copying text out (share-out policy). */
  selectable: boolean;
  loadingMore: boolean;
  onDelete: (entry: AgentMemoryEntry) => void;
  onOptOut: (optedOut: boolean) => void;
  onEndReached: () => void;
  onRetry: () => void;
  refreshControl?: ReactElement<RefreshControlProps>;
}

const KIND: Record<string, string> = { fact: 'Fact', preference: 'Preference', episode: 'Episode', cursor: 'Heartbeat state' };
const STATUS: Record<string, string> = { pending: 'Waiting for approval', active: 'Active', archived: 'Archived' };

export const kindLabel = (kind: string): string => KIND[kind] ?? kind;
export const statusLabel = (status: string): string => STATUS[status] ?? status;

/** Who wrote a note's current version: "The agent", a named person, or "A person". */
export function writerLabel(entry: Pick<AgentMemoryEntry, 'writtenByKind' | 'writtenByUserId'>, people: Readonly<Record<number, string>>, selfUserId: number | null): string {
  if (entry.writtenByKind === 'agent') return 'The agent';
  if (entry.writtenByUserId !== null && entry.writtenByUserId === selfUserId) return 'You';
  return (entry.writtenByUserId !== null ? people[entry.writtenByUserId] : undefined) ?? 'A person';
}

/** Who a note is about, or null for a note about the project or the work. */
export function subjectLabel(entry: Pick<AgentMemoryEntry, 'subjectUserId'>, people: Readonly<Record<number, string>>, selfUserId: number | null): string | null {
  if (entry.subjectUserId === null) return null;
  if (entry.subjectUserId === selfUserId) return 'About you';
  return `About ${people[entry.subjectUserId] ?? 'a person'}`;
}

/** The line under a note: kind, status (unless active), pinned, who it is about, who wrote it and when. */
export function entryMeta(entry: AgentMemoryEntry, people: Readonly<Record<number, string>>, selfUserId: number | null, now: number = Date.now()): string {
  const when = parseServerTime(entry.createdAt);
  return [
    kindLabel(entry.kind),
    entry.status === 'active' ? null : statusLabel(entry.status),
    entry.pinned ? 'Pinned' : null,
    subjectLabel(entry, people, selfUserId),
    writerLabel(entry, people, selfUserId),
    when === null ? null : formatShortDate(when, now),
  ]
    .filter(Boolean)
    .join(' · ');
}

/**
 * What one agent remembers in this project (ADR-0050 decision 1b, client contract 1.44): the
 * "Remember things about me" switch, then the notes the caller may see, newest pinned first as
 * the server orders them. Each note says what it is, who it is about, who wrote it and when; a
 * note the caller may delete has a Delete button (it asks first). Online only.
 *
 * The notes are a virtualised `FlatList` (issue 1839), as the chat lists are: an agent with
 * hundreds of notes, paged 50 at a time, mounts only the rows near the screen, and the next page
 * is asked for by `onEndReached`. The banner, the switch and the section title are the list's
 * header; the section note and the paging spinner its footer. The rows draw the inset group of
 * `ListSection` themselves (first and last rounded, hairline dividers between).
 */
export function AgentMemoryView({
  agentName,
  status,
  rows,
  total,
  people,
  selfUserId,
  canDelete,
  deleting,
  optedOut,
  optOutBusy,
  banner,
  selectable,
  loadingMore,
  onDelete,
  onOptOut,
  onEndReached,
  onRetry,
  refreshControl,
}: AgentMemoryViewProps) {
  const { colors, radius, scheme } = useDesign();
  const margin = useLayoutMargin();

  if (status.kind === 'unavailable') {
    return (
      <View style={[styles.flex, { backgroundColor: colors.background }]}>
        <EmptyState mark="small" title="Agent memory isn't available" body={status.message} />
      </View>
    );
  }
  if (status.kind === 'loading') {
    return (
      <View style={[styles.flex, styles.center, { backgroundColor: colors.background }]}>
        <ActivityIndicator color={colors.accent} accessibilityLabel="Loading" />
      </View>
    );
  }
  if (status.kind === 'error' && rows.length === 0) {
    return (
      <ScrollView style={{ backgroundColor: colors.background }} contentContainerStyle={styles.grow} refreshControl={refreshControl}>
        <EmptyState mark="none" title="Couldn't load the memory" body={status.message} action={{ title: 'Try again', onPress: onRetry }} />
      </ScrollView>
    );
  }

  const header = (
    <View style={styles.header}>
      {banner ? (
        <View accessible accessibilityRole="alert" style={[styles.banner, { backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="callout" tone="danger" style={styles.flex}>
            {banner}
          </Text>
        </View>
      ) : null}
      <ListSection
        footer={
          optedOut
            ? `${agentName} doesn't remember anything about you. Turn this back on to let it.`
            : `Turning this off deletes what ${agentName} remembers about you in this project, and it saves no more.`
        }>
        <ListRow
          title="Remember things about me"
          trailing={
            <Switch
              testID="agent-memory-opt-out"
              accessibilityLabel="Remember things about me"
              value={!optedOut}
              disabled={optOutBusy}
              onValueChange={(on) => onOptOut(!on)}
              trackColor={{ true: colors.accent }}
            />
          }
        />
      </ListSection>
      {rows.length > 0 ? (
        <Text
          heading
          variant={uiPlatform === 'ios' ? 'footnote' : 'subheadline'}
          tone={uiPlatform === 'ios' ? 'secondary' : 'accent'}
          style={styles.sectionTitle}>
          {sectionTitle(total === 1 ? '1 note' : `${total} notes`)}
        </Text>
      ) : null}
    </View>
  );
  const footer = (
    <View style={styles.footer}>
      {rows.length > 0 ? (
        <Text variant="footnote" tone="secondary" style={styles.sectionNote}>
          {SECTION_NOTE}
        </Text>
      ) : null}
      {loadingMore ? <ActivityIndicator color={colors.accent} /> : null}
    </View>
  );
  const edge = scheme === 'light' ? StyleSheet.hairlineWidth : 0;
  const group = { backgroundColor: colors.surface, borderColor: colors.separator, borderLeftWidth: edge, borderRightWidth: edge };

  return (
    <FlatList
      data={rows}
      keyExtractor={(entry) => String(entry.id)}
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      testID="agent-memory-list"
      contentContainerStyle={[styles.content, { paddingHorizontal: margin }]}
      refreshControl={refreshControl}
      onEndReached={onEndReached}
      onEndReachedThreshold={0.5}
      ListHeaderComponent={header}
      ListFooterComponent={footer}
      ListEmptyComponent={
        <EmptyState
          mark="none"
          title="Nothing remembered yet"
          body={`When ${agentName} keeps a note in this project, it appears here. Notes about other people are shown only to them and the agent's managers.`}
        />
      }
      ItemSeparatorComponent={() => (
        <View style={group}>
          <View style={[styles.divider, { backgroundColor: colors.separator }]} />
        </View>
      )}
      renderItem={({ item: entry, index }) => {
        const meta = entryMeta(entry, people, selfUserId);
        const first = index === 0;
        const last = index === rows.length - 1;
        return (
          <View
            testID={`agent-memory-${entry.id}`}
            style={[
              styles.row,
              group,
              first ? { borderTopWidth: edge, borderTopLeftRadius: radius.listRow, borderTopRightRadius: radius.listRow } : null,
              last ? { borderBottomWidth: edge, borderBottomLeftRadius: radius.listRow, borderBottomRightRadius: radius.listRow } : null,
            ]}>
            <View accessible accessibilityLabel={`${entry.content}, ${meta}`} style={styles.text}>
              <Text variant="body" selectable={selectable} tone={entry.status === 'active' ? 'primary' : 'secondary'}>
                {entry.content}
              </Text>
              <Text variant="caption" tone="secondary">
                {meta}
              </Text>
            </View>
            {canDelete(entry) ? (
              <Button
                title="Delete"
                variant="destructivePlain"
                size="small"
                accessibilityLabel={`Delete note: ${entry.content.slice(0, 80)}`}
                loading={deleting === entry.id}
                disabled={deleting !== null && deleting !== entry.id}
                onPress={() => onDelete(entry)}
                style={styles.end}
              />
            ) : null}
          </View>
        );
      }}
    />
  );
}

const SECTION_NOTE =
  "What this agent keeps across conversations in this project. Notes about other people are shown only to them and the agent's managers.";

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { alignItems: 'center', justifyContent: 'center' },
  grow: { flexGrow: 1 },
  content: { paddingVertical: 16, paddingBottom: 40 },
  header: { gap: 28, paddingBottom: 6 },
  sectionTitle: { paddingHorizontal: 16 },
  footer: { gap: 28, paddingTop: 6 },
  sectionNote: { paddingHorizontal: 16 },
  divider: { height: StyleSheet.hairlineWidth, marginStart: 16 },
  row: { overflow: 'hidden', borderCurve: 'continuous', paddingHorizontal: 16, paddingTop: 12, paddingBottom: 8, gap: 4 },
  text: { gap: 6 },
  end: { alignSelf: 'flex-end' },
  banner: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12, borderCurve: 'continuous' },
});
