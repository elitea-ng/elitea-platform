import { useQueryClient } from '@tanstack/react-query';
import { router, useLocalSearchParams } from 'expo-router';
import { useEffect, useRef, useState } from 'react';
import { ActivityIndicator, Pressable, SectionList, StyleSheet, Text, TextInput, View } from 'react-native';

import { StartConversationError, startConversationWithAgent, useAgents, type Agent } from '@/api/queries/agents';
import { queryKeys } from '@/api/queries/keys';
import { startConversationWithModel, useModels, type ChatModel } from '@/api/queries/models';
import { speaksContract } from '@/api/discovery';
import { useRecommendations } from '@/api/queries/recommendations';
import { StatusScreen } from '@/components/StatusScreen';
import { FONT_SCALE } from '@/constants/typeScale';
import { SuggestionStrip } from '@/features/chat/SuggestionStrip';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useSession } from '@/state/session';
import { useLockCovers } from '@/security/lock/useLockController';
import { markAgentStarted, readHandoff } from '@/features/intents/intentHandoff';
import { MAX_PREFILL } from '@/features/intents/intentRouter';

type Choice = { kind: 'agent'; key: string; agent: Agent } | { kind: 'model'; key: string; model: ChatModel };

/**
 * New conversation: pick an agent, or a model for a plain model chat (no
 * agent; its turns use the adhoc contract), create the conversation, open it.
 *
 * Opened by Siri / Shortcuts (src/features/intents/) with `intent`, the id
 * of an in-memory hand-over (src/features/intents/intentHandoff.ts), never
 * with the agent or the text as route params: a URL any app can build starts
 * nothing. Its agent's conversation starts once, when the agent is in the
 * selected project's list and the app is unlocked; its question is carried
 * into the new conversation's composer (never sent by itself).
 */
export default function NewConversationScreen() {
  const palette = usePalette();
  const queryClient = useQueryClient();
  const projectId = useSession((s) => s.projectId);
  const { intent } = useLocalSearchParams<{ intent?: string }>();
  const handoff = readHandoff(intent);
  const prefill = handoff?.prefill != null && handoff.prefill.trim() !== '' ? handoff.prefill.slice(0, MAX_PREFILL) : null;
  const wantedAgent = handoff?.agent?.agentId ?? null;
  const wantedProject = handoff?.agent?.projectId ?? null;
  const lockCovers = useLockCovers();
  const autoStarted = useRef(false);
  const [search, setSearch] = useState('');
  const agents = useAgents(projectId, search);
  const models = useModels(projectId);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  // Suggested agents (client contract 1.3) on the empty screen: no search typed.
  const workspace = useWorkspaceInfo();
  const contract13 = speaksContract(workspace?.discovery, 1, 3);
  const suggested = useRecommendations(projectId, contract13);
  const [error, setError] = useState<string | null>(null);

  const start = async (choice: Choice) => {
    if (projectId === null || busyKey !== null) return;
    setBusyKey(choice.key);
    setError(null);
    try {
      const created =
        choice.kind === 'agent'
          ? await startConversationWithAgent(projectId, choice.agent)
          : await startConversationWithModel(projectId, choice.model);
      void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      router.replace({
        pathname: '/c/[id]',
        params: prefill !== null && typeof intent === 'string' ? { id: String(created.id), intent } : { id: String(created.id) },
      });
    } catch (e) {
      setError(
        e instanceof StartConversationError && e.reason === 'no_version'
          ? 'This agent has no published version yet.'
          : "Couldn't start the conversation. Try again.",
      );
      setBusyKey(null);
    }
  };

  // "New chat with <agent>": start it once the list has it (an agent of another project, or
  // one no longer listed, leaves the picker open) and only while the app is unlocked.
  const listed = wantedAgent !== null && wantedProject === projectId ? agents.data?.find((a) => a.id === wantedAgent) : undefined;
  useEffect(() => {
    if (autoStarted.current || listed === undefined || lockCovers || search !== '') return;
    autoStarted.current = true;
    markAgentStarted(intent);
    void start({ kind: 'agent', key: `agent:${listed.id}`, agent: listed });
  });

  if (projectId === null) return <StatusScreen title="No project selected" />;

  const needle = search.trim().toLowerCase();
  const modelChoices: Choice[] = (models.data?.models ?? [])
    .filter((m) => needle === '' || m.displayName.toLowerCase().includes(needle) || m.name.toLowerCase().includes(needle))
    .map((model) => ({ kind: 'model', key: `model:${model.projectId}:${model.name}`, model }));
  const agentChoices: Choice[] = (agents.data ?? []).map((agent) => ({ kind: 'agent', key: `agent:${agent.id}`, agent }));
  const sections = [
    ...(agentChoices.length > 0 ? [{ title: 'Agents', data: agentChoices }] : []),
    ...(modelChoices.length > 0 ? [{ title: 'Chat with a model', data: modelChoices }] : []),
  ];
  const suggestions = needle === '' ? (suggested.data ?? []) : [];
  const loading = agents.isLoading || models.isLoading;
  const failed = agents.error && models.error;

  return (
    <View style={[styles.flex, { backgroundColor: palette.background }]}>
      <TextInput
        value={search}
        onChangeText={setSearch}
        placeholder="Search agents and models"
        placeholderTextColor={palette.textSecondary}
        autoCorrect={false}
        accessibilityLabel="Search agents and models"
        style={[styles.search, { color: palette.text, backgroundColor: palette.backgroundElement }]}
      />
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      {loading && sections.length === 0 ? (
        <ActivityIndicator style={styles.loading} color={palette.accent} />
      ) : failed ? (
        <Pressable
          accessibilityRole="button"
          onPress={() => {
            void agents.refetch();
            void models.refetch();
          }}
          style={styles.loading}>
          <Text style={{ color: palette.accent }}>Couldn&apos;t load agents or models. Retry</Text>
        </Pressable>
      ) : (
        <SectionList
          sections={sections}
          keyExtractor={(choice) => choice.key}
          keyboardShouldPersistTaps="handled"
          stickySectionHeadersEnabled={false}
          ListHeaderComponent={
            suggestions.length > 0 ? (
              <View style={styles.suggested}>
                <Text accessibilityRole="header" maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.header, { color: palette.textSecondary }]}>
                  Suggested
                </Text>
                <View style={styles.suggestedStrip}>
                  <SuggestionStrip
                    inline
                    label="Suggested agents"
                    state={{
                      items: suggestions.map((agent) => ({
                        key: `suggested:${agent.id}`,
                        title: agent.name,
                        subtitle: agent.description || 'Agent',
                        badge: busyKey === `suggested:${agent.id}` ? 'Starting…' : null,
                      })),
                    }}
                    onPick={(item) => {
                      const agent = suggestions.find((a) => `suggested:${a.id}` === item.key);
                      if (agent) void start({ kind: 'agent', key: item.key, agent });
                    }}
                  />
                </View>
              </View>
            ) : undefined
          }
          ListEmptyComponent={
            <Text style={[styles.empty, { color: palette.textSecondary }]}>No agents or models found.</Text>
          }
          renderSectionHeader={({ section }) => (
            <Text accessibilityRole="header" style={[styles.header, { color: palette.textSecondary }]}>
              {section.title}
            </Text>
          )}
          ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
          renderItem={({ item }) => {
            const title = item.kind === 'agent' ? item.agent.name : item.model.displayName;
            const subtitle =
              item.kind === 'agent'
                ? item.agent.description
                : item.model.isDefault
                  ? 'Default model'
                  : item.model.displayName !== item.model.name
                    ? item.model.name
                    : '';
            return (
              <Pressable
                accessibilityRole="button"
                accessibilityState={{ busy: busyKey === item.key, disabled: busyKey !== null }}
                disabled={busyKey !== null}
                onPress={() => void start(item)}
                style={styles.row}>
                <View style={styles.flex}>
                  <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
                    {title}
                  </Text>
                  {subtitle ? (
                    <Text numberOfLines={2} style={[styles.description, { color: palette.textSecondary }]}>
                      {subtitle}
                    </Text>
                  ) : null}
                </View>
                {busyKey === item.key ? <ActivityIndicator color={palette.accent} /> : null}
              </Pressable>
            );
          }}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  search: { margin: Spacing.three, borderRadius: 10, paddingHorizontal: Spacing.three, paddingVertical: Spacing.two, fontSize: 16 },
  loading: { padding: Spacing.four, alignItems: 'center' },
  error: { paddingHorizontal: Spacing.three, fontSize: 14 },
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15 },
  header: { fontSize: 13, fontWeight: '600', paddingHorizontal: Spacing.three, paddingTop: Spacing.three, paddingBottom: Spacing.one },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  row: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two, padding: Spacing.three },
  suggested: { paddingBottom: Spacing.two },
  suggestedStrip: { paddingHorizontal: Spacing.three },
  name: { fontSize: 16, fontWeight: '500' },
  description: { fontSize: 13, marginTop: 2 },
});
