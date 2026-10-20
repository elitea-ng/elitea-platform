import { useQueryClient } from '@tanstack/react-query';
import { router, useLocalSearchParams } from 'expo-router';
import { useEffect, useRef, useState } from 'react';
import { ActivityIndicator, SectionList, StyleSheet, TextInput, View } from 'react-native';

import { StartConversationError, startConversationWithAgent, useAgents, type Agent } from '@/api/queries/agents';
import { queryKeys } from '@/api/queries/keys';
import { startConversationWithModel, useModels, type ChatModel } from '@/api/queries/models';
import { speaksContract } from '@/api/discovery';
import { useRecommendations } from '@/api/queries/recommendations';
import { StatusScreen } from '@/components/StatusScreen';
import { SuggestionStrip } from '@/features/chat/SuggestionStrip';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { useDesign } from '@/design/DesignProvider';
import { useSession } from '@/state/session';
import { useLockCovers } from '@/security/lock/useLockController';
import { markAgentStarted, readHandoff } from '@/features/intents/intentHandoff';
import { MAX_PREFILL } from '@/features/intents/intentRouter';
import { Avatar, EmptyState, hitTarget, Icon, IconTile, ListRow, maxScale, ramp, sectionTitle, Text, uiPlatform, useLayoutMargin } from '@/ui';

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
  const { colors, radius } = useDesign();
  const margin = useLayoutMargin();
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
        choice.kind === 'agent' ? await startConversationWithAgent(projectId, choice.agent) : await startConversationWithModel(projectId, choice.model);
      void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      router.replace({
        pathname: '/c/[id]',
        // The hand-over travels on when it still has something for the conversation to do.
        params: (prefill !== null || handoff?.then) && typeof intent === 'string' ? { id: String(created.id), intent } : { id: String(created.id) },
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
    <View style={[styles.flex, { backgroundColor: colors.background }]}>
      <View
        style={[
          styles.search,
          { marginHorizontal: margin, backgroundColor: colors.surfaceSunken, borderColor: colors.separatorStrong, borderRadius: radius.md },
        ]}>
        <Icon name="search" size={18} tone="secondary" />
        <TextInput
          value={search}
          onChangeText={setSearch}
          placeholder="Search agents and models"
          placeholderTextColor={colors.textTertiary}
          autoCorrect={false}
          autoCapitalize="none"
          clearButtonMode="while-editing"
          returnKeyType="search"
          accessibilityLabel="Search agents and models"
          maxFontSizeMultiplier={maxScale.body}
          style={[ramp.body, styles.flex, { color: colors.textPrimary, minHeight: hitTarget() }]}
        />
      </View>
      {error ? (
        <View
          accessible
          accessibilityRole="alert"
          style={[styles.error, { marginHorizontal: margin, backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="callout" tone="danger" style={styles.flex}>
            {error}
          </Text>
        </View>
      ) : null}
      {loading && sections.length === 0 ? (
        <ActivityIndicator style={styles.loading} color={colors.accent} />
      ) : failed ? (
        <EmptyState
          mascot="none"
          title="Couldn't load agents or models"
          body="Check your connection and try again."
          action={{
            title: 'Try again',
            onPress: () => {
              void agents.refetch();
              void models.refetch();
            },
          }}
        />
      ) : (
        <SectionList
          sections={sections}
          keyExtractor={(choice) => choice.key}
          keyboardShouldPersistTaps="handled"
          keyboardDismissMode="on-drag"
          stickySectionHeadersEnabled={false}
          contentContainerStyle={[styles.list, { paddingHorizontal: margin }]}
          ListHeaderComponent={
            suggestions.length > 0 ? (
              <View style={styles.suggested}>
                <Text heading variant="footnote" tone="secondary" emphasized style={styles.sectionHeader}>
                  Suggested
                </Text>
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
            ) : undefined
          }
          ListEmptyComponent={
            needle !== '' ? (
              <EmptyState mascot="bust" title="No agents or models found" body={`Nothing matches “${search.trim()}”.`} />
            ) : (
              <EmptyState mascot="bust" title="No agents or models found" body="This project has no agents or models you can chat with yet." />
            )
          }
          renderSectionHeader={({ section }) => (
            <Text
              heading
              variant={uiPlatform === 'ios' ? 'footnote' : 'subheadline'}
              tone={uiPlatform === 'ios' ? 'secondary' : 'accent'}
              style={styles.sectionHeader}>
              {sectionTitle(section.title)}
            </Text>
          )}
          renderItem={({ item, index, section }) => {
            const title = item.kind === 'agent' ? item.agent.name : item.model.displayName;
            const subtitle =
              item.kind === 'agent'
                ? item.agent.description
                : item.model.isDefault
                  ? 'Default model'
                  : item.model.displayName !== item.model.name
                    ? item.model.name
                    : '';
            const first = index === 0;
            const last = index === section.data.length - 1;
            return (
              <View
                style={[
                  styles.cell,
                  { backgroundColor: colors.surface },
                  first ? { borderTopLeftRadius: radius.listRow, borderTopRightRadius: radius.listRow } : null,
                  last ? { borderBottomLeftRadius: radius.listRow, borderBottomRightRadius: radius.listRow, marginBottom: 20 } : null,
                ]}>
                {first ? null : <View style={[styles.divider, { backgroundColor: colors.separator }]} />}
                <ListRow
                  title={title}
                  subtitle={subtitle || null}
                  numberOfLines={1}
                  leading={
                    item.kind === 'agent' ? <Avatar name={title} kind="agent" size={36} decorative /> : <IconTile icon="cpu" size={36} tone="secondary" />
                  }
                  busy={busyKey === item.key}
                  // The row that is starting stays at full opacity with its spinner (F4); start()
                  // refuses a second tap while busyKey is set.
                  disabled={busyKey !== null && busyKey !== item.key}
                  trailing={busyKey === item.key ? <ActivityIndicator color={colors.accent} /> : undefined}
                  onPress={() => void start(item)}
                />
              </View>
            );
          }}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  search: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, marginVertical: 12, borderWidth: 1, borderCurve: 'continuous' },
  loading: { padding: 32, alignItems: 'center' },
  error: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12, marginBottom: 12, borderCurve: 'continuous' },
  list: { paddingBottom: 32, flexGrow: 1 },
  sectionHeader: { paddingHorizontal: 16, paddingBottom: 6 },
  suggested: { paddingBottom: 20, gap: 6 },
  cell: { overflow: 'hidden', borderCurve: 'continuous' },
  divider: { height: StyleSheet.hairlineWidth, marginStart: 64 },
});
