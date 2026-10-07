import { useQueryClient } from '@tanstack/react-query';
import { router, useLocalSearchParams } from 'expo-router';
import { useRef, useState } from 'react';
import { ActivityIndicator, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import {
  StartPublicChatError,
  startConversationWithPublicAgent,
  startPublicChatErrorText,
  useCachedPublicAgent,
  usePublicAgentDetail,
} from '@/api/queries/discover';
import { queryKeys } from '@/api/queries/keys';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { AgentIcon } from '@/features/discover/AgentIcon';
import { TagList } from '@/features/discover/TagList';
import { useDiscoverAvailable } from '@/features/discover/useDiscoverAvailable';
import { handOff } from '@/features/intents/intentHandoff';
import { MAX_PREFILL } from '@/features/intents/intentRouter';
import { useProjectLabel } from '@/features/projects/ProjectChip';
import { useProjectSelection } from '@/features/projects/useProjectSelection';

/**
 * One public agent: what it is, and a chat with it in the ACTIVE project
 * (createConversation there, then the agent as a participant at its
 * published version). A conversation starter opens the new chat with that
 * text in the composer; it is never sent by itself.
 */
export default function PublicAgentScreen() {
  const palette = usePalette();
  const queryClient = useQueryClient();
  const available = useDiscoverAvailable();
  const { projectId } = useProjectSelection();
  const projectLabel = useProjectLabel();
  const { id } = useLocalSearchParams<{ id: string }>();
  const agentId = typeof id === 'string' && /^\d{1,15}$/.test(id) ? Number(id) : null;
  const agent = useCachedPublicAgent(projectId, agentId);
  const detail = usePublicAgentDetail(projectId, agent);
  const [busy, setBusy] = useState<string | null>(null);
  // Set synchronously: a second touch in the same frame sees it before `busy` (or the
  // button's disabled state) has rendered, so one tap creates one conversation.
  const starting = useRef(false);
  const [error, setError] = useState<string | null>(null);

  if (!available) return <StatusScreen title="Discover isn't available" />;
  if (projectId === null) return <StatusScreen title="No project selected" />;
  if (agent === null) {
    return (
      <StatusScreen title="Agent not found" body="It is no longer in the list. Go back and search again.">
        <Pressable accessibilityRole="button" onPress={() => router.back()}>
          <Text style={[styles.link, { color: palette.accent }]}>Back to Discover</Text>
        </Pressable>
      </StatusScreen>
    );
  }

  const start = async (key: string, prefill: string | null) => {
    if (starting.current) return;
    starting.current = true;
    setBusy(key);
    setError(null);
    try {
      const created = await startConversationWithPublicAgent(projectId, agent);
      void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      const params: { id: string; intent?: string } = { id: String(created.id) };
      if (prefill !== null) params.intent = handOff({ agent: null, prefill: prefill.slice(0, MAX_PREFILL) });
      router.replace({ pathname: '/c/[id]', params });
    } catch (e) {
      // A conversation that could not be deleted again shows in the list now, not at some later refetch.
      if (e instanceof StartPublicChatError && e.leftover) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      }
      setError(startPublicChatErrorText(e));
      starting.current = false;
      setBusy(null);
    }
  };

  const welcome = detail.data?.welcomeMessage ?? null;
  const starters = detail.data?.starters ?? [];

  return (
    <ScrollView style={{ backgroundColor: palette.background }} contentContainerStyle={styles.content}>
      <View style={styles.header}>
        <AgentIcon name={agent.name} iconPath={agent.iconPath} projectId={projectId} size={64} />
        <View style={styles.headerText}>
          <Text accessibilityRole="header" style={[styles.title, { color: palette.text }]}>
            {agent.name}
          </Text>
          <Text style={[styles.meta, { color: palette.textSecondary }]}>
            {[agent.agentType === 'pipeline' ? 'Pipeline' : 'Agent', agent.versionName ? `version ${agent.versionName}` : null]
              .filter(Boolean)
              .join(' · ')}
          </Text>
        </View>
      </View>
      <TagList category={agent.category} tags={agent.tags} />
      {agent.description ? <Text style={[styles.body, { color: palette.text }]}>{agent.description}</Text> : null}
      {welcome ? (
        <View style={[styles.welcome, { backgroundColor: palette.backgroundElement }]}>
          <Text style={[styles.body, { color: palette.text }]}>{welcome}</Text>
        </View>
      ) : null}
      {error ? (
        <Text accessibilityRole="alert" style={[styles.body, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      <Pressable
        testID="start-public-chat"
        accessibilityRole="button"
        accessibilityLabel={`Start a chat in ${projectLabel}`}
        accessibilityState={{ busy: busy === 'start', disabled: busy !== null }}
        disabled={busy !== null}
        onPress={() => void start('start', null)}
        style={[styles.primary, { backgroundColor: palette.accent, opacity: busy !== null && busy !== 'start' ? 0.5 : 1 }]}>
        {busy === 'start' ? (
          <ActivityIndicator color={palette.onAccent} />
        ) : (
          <Text style={[styles.primaryText, { color: palette.onAccent }]}>Start chat</Text>
        )}
      </Pressable>
      <Text style={[styles.note, { color: palette.textSecondary }]}>The conversation is created in {projectLabel}.</Text>
      {starters.length > 0 ? (
        <View style={styles.starters}>
          <Text accessibilityRole="header" style={[styles.section, { color: palette.textSecondary }]}>
            Start with
          </Text>
          {starters.map((starter, index) => (
            <Pressable
              key={starter}
              testID={`starter-${index}`}
              accessibilityRole="button"
              accessibilityHint="Starts a chat with this text in the message box"
              accessibilityState={{ busy: busy === `starter:${index}`, disabled: busy !== null }}
              disabled={busy !== null}
              onPress={() => void start(`starter:${index}`, starter)}
              style={[styles.starter, { borderColor: palette.border }]}>
              <Text style={[styles.starterText, { color: palette.text }]}>{starter}</Text>
              {busy === `starter:${index}` ? <ActivityIndicator color={palette.accent} /> : null}
            </Pressable>
          ))}
        </View>
      ) : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { padding: Spacing.three, gap: Spacing.three },
  header: { flexDirection: 'row', alignItems: 'center', gap: Spacing.three },
  headerText: { flex: 1, gap: 2 },
  title: { fontSize: 22, fontWeight: '600' },
  meta: { fontSize: 13 },
  body: { fontSize: 15, lineHeight: 21 },
  welcome: { borderRadius: 12, padding: Spacing.three },
  primary: { borderRadius: 12, paddingVertical: Spacing.three, alignItems: 'center' },
  primaryText: { fontSize: 16, fontWeight: '600' },
  note: { fontSize: 13, textAlign: 'center', marginTop: -Spacing.two },
  starters: { gap: Spacing.two },
  section: { fontSize: 13, fontWeight: '600' },
  starter: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two, borderWidth: 1, borderRadius: 12, padding: Spacing.three },
  starterText: { flex: 1, fontSize: 15 },
  link: { fontSize: 16, fontWeight: '600' },
});
