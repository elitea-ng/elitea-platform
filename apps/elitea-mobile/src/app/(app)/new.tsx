import { useQueryClient } from '@tanstack/react-query';
import { router } from 'expo-router';
import { useState } from 'react';
import { ActivityIndicator, Pressable, SectionList, StyleSheet, Text, TextInput, View } from 'react-native';

import { StartConversationError, startConversationWithAgent, useAgents, type Agent } from '@/api/queries/agents';
import { queryKeys } from '@/api/queries/keys';
import { startConversationWithModel, useModels, type ChatModel } from '@/api/queries/models';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useSession } from '@/state/session';

type Choice = { kind: 'agent'; key: string; agent: Agent } | { kind: 'model'; key: string; model: ChatModel };

/**
 * New conversation: pick an agent, or a model for a plain model chat (no
 * agent; its turns use the adhoc contract), create the conversation, open it.
 */
export default function NewConversationScreen() {
  const palette = usePalette();
  const queryClient = useQueryClient();
  const projectId = useSession((s) => s.projectId);
  const [search, setSearch] = useState('');
  const agents = useAgents(projectId, search);
  const models = useModels(projectId);
  const [busyKey, setBusyKey] = useState<string | null>(null);
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
      router.replace({ pathname: '/c/[id]', params: { id: String(created.id) } });
    } catch (e) {
      setError(
        e instanceof StartConversationError && e.reason === 'no_version'
          ? 'This agent has no published version yet.'
          : "Couldn't start the conversation. Try again.",
      );
      setBusyKey(null);
    }
  };

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
  name: { fontSize: 16, fontWeight: '500' },
  description: { fontSize: 13, marginTop: 2 },
});
