import { useMemo, useState, type ReactElement } from 'react';
import { ActivityIndicator, FlatList, Pressable, ScrollView, StyleSheet, useWindowDimensions, View, type RefreshControlProps } from 'react-native';

import type { PublicAgent } from '@/api/queries/discover';
import { useDesign } from '@/design/DesignProvider';
import { AgentIcon } from '@/features/discover/AgentIcon';
import { TagList } from '@/features/discover/TagList';
import { ArrowButton, Chip, EmptyState, ListRow, pressedOverlay, ripple, Text, uiPlatform, useLayoutMargin } from '@/ui';

export type DiscoverLayout = 'grid' | 'list';

/** "All" plus each category the loaded agents carry, most frequent first; none for fewer than two. */
export function categoryFilters(agents: readonly Pick<PublicAgent, 'category'>[]): string[] {
  const counts = new Map<string, number>();
  for (const agent of agents) if (agent.category) counts.set(agent.category, (counts.get(agent.category) ?? 0) + 1);
  if (counts.size < 2) return [];
  return [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([category]) => category);
}

/** How many card columns fit: two on a phone, more on an iPad or tablet, one at accessibility sizes. */
export function gridColumns(width: number, margin: number, accessibilityLayout: boolean): number {
  if (accessibilityLayout) return 1;
  const usable = Math.min(width, 900) - margin * 2;
  return Math.max(2, Math.min(4, Math.floor((usable + 12) / (168 + 12))));
}

/** One agent's spoken label: its name, its category and the start of its description. */
export function agentCardLabel(agent: Pick<PublicAgent, 'name' | 'category' | 'description' | 'tags'>): string {
  return [
    agent.name,
    agent.category,
    agent.description ? agent.description.slice(0, 160) : null,
    agent.tags.length > 0 ? `tags ${agent.tags.map((t) => t.name).join(', ')}` : null,
  ]
    .filter(Boolean)
    .join(', ');
}

export type DiscoverStatus = 'loading' | 'error' | 'ready';

export interface DiscoverViewProps {
  agents: readonly PublicAgent[];
  /** The active project (the icon cache lives under it). */
  projectId: number | null;
  layout: DiscoverLayout;
  status: DiscoverStatus;
  /** The search as sent to the server (trimmed); '' when not searching. */
  query: string;
  onOpen: (agent: PublicAgent) => void;
  onRetry: () => void;
  /** Above the list (the workspace pill). */
  header?: ReactElement;
  refreshControl?: ReactElement<RefreshControlProps>;
  onEndReached?: () => void;
  loadingMore?: boolean;
}

/**
 * The Discover tab (docs/screens.md §6): the workspace's public agents as glass-free cards in a
 * grid (2 columns on a phone, up to 4 on an iPad; a single column at accessibility text sizes)
 * or as an inset-grouped list, with category filter chips when the catalogue has more than one
 * category. Each card is ONE button (its arrow is a visual), labelled by the agent. The tab route
 * and its `__DEV__` preview both render this (D2). The mascot appears only in the empty state:
 * a catalogue full of icon-less agents gets monograms, never a wall of Zefircas (§5).
 */
export function DiscoverView({
  agents,
  projectId,
  layout,
  status,
  query,
  onOpen,
  onRetry,
  header,
  refreshControl,
  onEndReached,
  loadingMore,
}: DiscoverViewProps) {
  const design = useDesign();
  const { colors, accessibilityLayout } = design;
  const margin = useLayoutMargin();
  const { width } = useWindowDimensions();
  const categories = useMemo(() => categoryFilters(agents), [agents]);
  const [picked, setPicked] = useState<string | null>(null);
  const category = picked !== null && categories.includes(picked) ? picked : null;
  const shown = category === null ? agents : agents.filter((a) => a.category === category);
  const columns = layout === 'grid' ? gridColumns(width, margin, accessibilityLayout) : 1;
  const cardWidth = (Math.min(width, 900) - margin * 2 - 12 * (columns - 1)) / columns;

  const filters =
    categories.length > 0 && status === 'ready' ? (
      <ScrollView
        testID="discover-filters"
        horizontal
        showsHorizontalScrollIndicator={false}
        accessibilityRole="tablist"
        // One row that scrolls sideways (a horizontal ScrollView grows by default: keep its height).
        style={styles.filterBar}
        contentContainerStyle={[styles.filters, { paddingHorizontal: margin }]}>
        <Chip label="All" selected={category === null} accessibilityRole="tab" onPress={() => setPicked(null)} />
        {categories.map((c) => (
          <Chip key={c} label={c} selected={category === c} accessibilityRole="tab" onPress={() => setPicked(c)} />
        ))}
      </ScrollView>
    ) : null;

  const empty =
    status === 'loading' ? (
      <ActivityIndicator style={styles.state} color={colors.accent} />
    ) : status === 'error' ? (
      <EmptyState
        mascot="none"
        title="Couldn't load public agents"
        body="Check your connection and try again."
        action={{ title: 'Try again', onPress: onRetry }}
      />
    ) : query !== '' ? (
      <EmptyState mascot="bust" title={`Nothing found for “${query}”`} body="Try another name or a word from the description." />
    ) : (
      <EmptyState mascot="bust" title="No public agents yet" body="This workspace has no public agents yet. Agents your team shares appear here." />
    );

  const data = status === 'ready' ? shown : [];
  return (
    <FlatList<PublicAgent>
      key={`columns-${columns}`}
      testID={layout === 'grid' ? 'discover-grid' : 'discover-list'}
      contentInsetAdjustmentBehavior="automatic"
      keyboardDismissMode="on-drag"
      keyboardShouldPersistTaps="handled"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={data.length === 0 ? styles.grow : styles.content}
      data={data}
      numColumns={columns}
      columnWrapperStyle={columns > 1 ? [styles.columns, { paddingHorizontal: margin }] : undefined}
      keyExtractor={(agent) => String(agent.id)}
      refreshControl={refreshControl}
      ListHeaderComponent={
        <>
          {header}
          {filters}
        </>
      }
      ListEmptyComponent={empty}
      ListFooterComponent={loadingMore ? <ActivityIndicator style={styles.state} color={colors.accent} /> : undefined}
      onEndReachedThreshold={0.5}
      onEndReached={onEndReached}
      renderItem={({ item, index }) =>
        layout === 'grid' ? (
          <View style={[columns === 1 ? { paddingHorizontal: margin } : { width: cardWidth }, styles.cell]}>
            <AgentCard agent={item} projectId={projectId} onPress={() => onOpen(item)} />
          </View>
        ) : (
          <AgentListRow agent={item} projectId={projectId} first={index === 0} last={index === data.length - 1} margin={margin} onPress={() => onOpen(item)} />
        )
      }
    />
  );
}

/**
 * The reference's card (§4.2 language): the agent's icon tile top-start, the circular arrow
 * top-end (visual only), the name (headline, two lines), two lines of description and the
 * category. `surface` with e1 on the flat ground; the whole card is one 44 pt+ button.
 */
export function AgentCard({ agent, projectId, onPress }: { agent: PublicAgent; projectId: number | null; onPress: () => void }) {
  const design = useDesign();
  const { colors, scheme, radius, space, accessibilityLayout } = design;
  return (
    <Pressable
      testID={`public-agent-${agent.id}`}
      accessibilityRole="button"
      accessibilityLabel={agentCardLabel(agent)}
      accessibilityHint="Opens the agent's details"
      android_ripple={ripple(design)}
      onPress={onPress}
      style={[
        styles.card,
        {
          padding: space.cardPadding,
          borderRadius: radius.card,
          backgroundColor: colors.surface,
          borderColor: colors.separator,
          borderWidth: scheme === 'light' ? StyleSheet.hairlineWidth : 0,
        },
        scheme === 'light' ? design.elevation.e1 : null,
      ]}>
      {({ pressed }) => (
        <>
          {pressed && uiPlatform === 'ios' ? (
            <View pointerEvents="none" style={[StyleSheet.absoluteFill, { backgroundColor: pressedOverlay(design) }]} />
          ) : null}
          <View style={styles.cardTop}>
            <AgentIcon name={agent.name} iconPath={agent.iconPath} projectId={projectId} size={44} />
            <ArrowButton size={36} />
          </View>
          <Text variant="headline" numberOfLines={accessibilityLayout ? undefined : 2}>
            {agent.name}
          </Text>
          {agent.description ? (
            <Text variant="footnote" tone="secondary" numberOfLines={accessibilityLayout ? 4 : 2}>
              {agent.description}
            </Text>
          ) : null}
          <View style={styles.cardFoot}>
            <TagList category={agent.category} tags={agent.tags} limit={2} />
          </View>
        </>
      )}
    </Pressable>
  );
}

/** One agent as an inset-grouped list row (the list layout). */
function AgentListRow({
  agent,
  projectId,
  first,
  last,
  margin,
  onPress,
}: {
  agent: PublicAgent;
  projectId: number | null;
  first: boolean;
  last: boolean;
  margin: number;
  onPress: () => void;
}) {
  const { colors, radius, scheme } = useDesign();
  return (
    <View
      style={[
        {
          marginHorizontal: margin,
          backgroundColor: colors.surface,
          borderColor: colors.separator,
          borderLeftWidth: scheme === 'light' ? StyleSheet.hairlineWidth : 0,
          borderRightWidth: scheme === 'light' ? StyleSheet.hairlineWidth : 0,
        },
        first
          ? {
              borderTopLeftRadius: radius.listRow,
              borderTopRightRadius: radius.listRow,
              borderTopWidth: scheme === 'light' ? StyleSheet.hairlineWidth : 0,
              marginTop: 8,
            }
          : null,
        last
          ? {
              borderBottomLeftRadius: radius.listRow,
              borderBottomRightRadius: radius.listRow,
              borderBottomWidth: scheme === 'light' ? StyleSheet.hairlineWidth : 0,
            }
          : null,
        styles.listRow,
      ]}>
      {first ? null : <View style={[styles.divider, { backgroundColor: colors.separator }]} />}
      <ListRow
        testID={`public-agent-${agent.id}`}
        title={agent.name}
        subtitle={agent.description || null}
        leading={<AgentIcon name={agent.name} iconPath={agent.iconPath} projectId={projectId} size={40} />}
        // The category and tags, as on a card (F5): the list layout is for scanning them too.
        footer={
          agent.category !== null || agent.tags.length > 0 ? (
            <View style={styles.listTags}>
              <TagList category={agent.category} tags={agent.tags} limit={3} />
            </View>
          ) : null
        }
        accessibilityLabel={agentCardLabel(agent)}
        accessibilityHint="Opens the agent's details"
        onPress={onPress}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  grow: { flexGrow: 1 },
  content: { paddingBottom: 32 },
  state: { padding: 32, alignItems: 'center' },
  filterBar: { flexGrow: 0, flexShrink: 0 },
  listTags: { paddingTop: 4 },
  filters: { alignItems: 'center', gap: 8, paddingBottom: 12, paddingTop: 4 },
  columns: { gap: 12 },
  cell: { marginBottom: 12 },
  card: { flex: 1, minHeight: 176, gap: 8, borderCurve: 'continuous', overflow: 'hidden' },
  cardTop: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 4 },
  cardFoot: { marginTop: 'auto', paddingTop: 4 },
  listRow: { overflow: 'hidden', borderCurve: 'continuous' },
  divider: { height: StyleSheet.hairlineWidth, marginStart: 68 },
});
