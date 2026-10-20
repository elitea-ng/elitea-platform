import { ActivityIndicator, ScrollView, StyleSheet, View } from 'react-native';

import type { PublicAgent } from '@/api/queries/discover';
import { useDesign } from '@/design/DesignProvider';
import { AgentIcon } from '@/features/discover/AgentIcon';
import { TagList } from '@/features/discover/TagList';
import { Button, Icon, ListRow, ListSection, Text } from '@/ui';

export interface PublicAgentViewProps {
  agent: PublicAgent;
  projectId: number | null;
  /** Where the chat is created ("My project", the project's name). */
  projectLabel: string;
  welcome: string | null;
  starters: readonly string[];
  /** The action running: `start`, `starter:<index>`, or null. */
  busy: string | null;
  error: string | null;
  onStart: () => void;
  onStarter: (index: number, text: string) => void;
}

/** "Agent · version v1" / "Pipeline". */
export function agentMetaLine(agent: Pick<PublicAgent, 'agentType' | 'versionName'>): string {
  return [agent.agentType === 'pipeline' ? 'Pipeline' : 'Agent', agent.versionName ? `version ${agent.versionName}` : null].filter(Boolean).join(' · ');
}

/**
 * One public agent, in a sheet over Discover (docs/screens.md §6): its icon (or monogram, never
 * the mascot: the sheet is about THIS agent), name, kind and version, category and tags, the
 * description and welcome message, ONE filled "Start chat" and the conversation starters as list
 * rows. The route and the `__DEV__` preview both render this (D2).
 */
export function PublicAgentView({ agent, projectId, projectLabel, welcome, starters, busy, error, onStart, onStarter }: PublicAgentViewProps) {
  const { colors, space, radius } = useDesign();
  return (
    <ScrollView
      style={{ backgroundColor: colors.elevated }}
      contentInsetAdjustmentBehavior="automatic"
      contentContainerStyle={[styles.content, { padding: space.screenGutterCompact }]}>
      <View style={styles.hero}>
        <AgentIcon name={agent.name} iconPath={agent.iconPath} projectId={projectId} size={72} />
        <Text variant="title2" heading style={styles.center}>
          {agent.name}
        </Text>
        <Text variant="footnote" tone="secondary" style={styles.center}>
          {agentMetaLine(agent)}
        </Text>
        <TagList category={agent.category} tags={agent.tags} on="elevated" />
      </View>
      {agent.description ? (
        <Text variant="body" selectable>
          {agent.description}
        </Text>
      ) : null}
      {welcome ? (
        <View style={[styles.welcome, { backgroundColor: colors.surfaceSunken, borderRadius: radius.lg }]}>
          <View style={styles.welcomeTitle}>
            <Icon name="chats" size={16} tone="secondary" />
            <Text variant="footnote" tone="secondary" emphasized>
              Welcome message
            </Text>
          </View>
          <Text variant="callout">{welcome}</Text>
        </View>
      ) : null}
      {error ? (
        <View accessible accessibilityRole="alert" style={[styles.error, { backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="callout" tone="danger" style={styles.flex}>
            {error}
          </Text>
        </View>
      ) : null}
      <View style={styles.actions}>
        <Button
          testID="start-public-chat"
          title="Start chat"
          icon="newChat"
          haptic="send"
          accessibilityLabel={`Start a chat in ${projectLabel}`}
          loading={busy === 'start'}
          disabled={busy !== null && busy !== 'start'}
          onPress={onStart}
        />
        <Text variant="footnote" tone="secondary" style={styles.center}>
          The conversation is created in {projectLabel}.
        </Text>
      </View>
      {starters.length > 0 ? (
        <ListSection title="Start with">
          {starters.map((starter, index) => (
            <ListRow
              key={starter}
              testID={`starter-${index}`}
              icon="sparkles"
              title={starter}
              numberOfLines={3}
              accessibilityHint="Starts a chat with this text in the message box"
              busy={busy === `starter:${index}`}
              // The starter that is running stays at full opacity with its spinner (F4).
              disabled={busy !== null && busy !== `starter:${index}`}
              trailing={busy === `starter:${index}` ? <ActivityIndicator color={colors.accent} /> : undefined}
              onPress={() => onStarter(index, starter)}
            />
          ))}
        </ListSection>
      ) : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { gap: 20, paddingBottom: 40 },
  hero: { alignItems: 'center', gap: 8, paddingTop: 8 },
  center: { textAlign: 'center' },
  welcome: { padding: 16, gap: 6, borderCurve: 'continuous' },
  welcomeTitle: { flexDirection: 'row', alignItems: 'center', gap: 6 },
  error: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12, borderCurve: 'continuous' },
  flex: { flex: 1 },
  actions: { gap: 8 },
});
