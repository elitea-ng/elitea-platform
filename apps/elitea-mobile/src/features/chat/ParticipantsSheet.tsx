import { useState } from 'react';
import { ActivityIndicator, Alert, ScrollView, StyleSheet, TextInput, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { useAgents } from '@/api/queries/agents';
import {
  addAgentParticipant,
  addPersonParticipant,
  participantErrorText,
  removeParticipant,
  useParticipantCandidates,
  type Participant,
} from '@/api/queries/participants';
import { useDesign } from '@/design/DesignProvider';
import { useModalAnimation } from '@/features/a11y/motion';
import { participantName, participantUserId } from '@/features/chat/messageModel';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { Avatar, Button, hitTarget, Icon, ListRow, ListSection, maxScale, ramp, Text } from '@/ui';

const KIND_LABEL: Record<string, string> = {
  user: 'Person',
  application: 'Agent',
  pipeline: 'Pipeline',
  toolkit: 'Toolkit',
  dummy: 'Model',
  llm: 'Model',
};

/** Who can be removed from mobile: people (not me) and agents. Toolkits and models are web-only. */
export function isRemovable(participant: Participant, selfUserId: number | null): boolean {
  if (participant.entity_name === 'user') return participantUserId(participant) !== selfUserId;
  return participant.entity_name === 'application' || participant.entity_name === 'pipeline';
}

export interface ParticipantsSheetProps {
  visible: boolean;
  onClose: () => void;
  projectId: number;
  /** The conversation's numeric id (the participant routes take it). */
  conversationId: string;
  participants: readonly Participant[];
  selfUserId: number | null;
  offline: boolean;
  /**
   * False next to toolkits: the server refuses a turn that mixes toolkits and
   * agents, so "Add agent" is replaced by a note saying why.
   */
  canAddAgent?: boolean;
  /** Re-read the conversation after a change. */
  onChanged: () => void;
  /** An agent was added: address it. */
  onAgentAdded?: (participantId: number) => void;
}

/**
 * The conversation's participants, opened from the header: who is in it,
 * remove (with a confirmation), "Add people" (project members, searched on
 * the server) and "Add agent". Online only.
 */
export function ParticipantsSheet(props: ParticipantsSheetProps) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={props.visible} animationType={animation} presentationStyle="pageSheet" onRequestClose={props.onClose}>
      {props.visible ? <SheetBody {...props} /> : null}
    </LockAwareModal>
  );
}

type Mode = 'list' | 'people' | 'agents';

function SheetBody({
  onClose,
  projectId,
  conversationId,
  participants,
  selfUserId,
  offline,
  canAddAgent = true,
  onChanged,
  onAgentAdded,
}: ParticipantsSheetProps) {
  const { colors, radius } = useDesign();
  const insets = useSafeAreaInsets();
  const [mode, setMode] = useState<Mode>('list');
  const [query, setQuery] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const candidates = useParticipantCandidates(projectId, conversationId, mode === 'people' ? query : '', !offline);
  const agents = useAgents(mode === 'agents' && !offline ? projectId : null, query);
  const peopleAvailable = candidates.data !== null;

  const run = (work: () => Promise<void>) => {
    if (busy || offline) return;
    setBusy(true);
    setError(null);
    work()
      .then(() => {
        onChanged();
        setMode('list');
        setQuery('');
      })
      .catch((e: unknown) => setError(participantErrorText(e)))
      .finally(() => setBusy(false));
  };

  const confirmRemove = (participant: Participant) => {
    const name = participantName(participant) ?? 'this participant';
    Alert.alert(`Remove ${name}?`, 'They will no longer be part of this conversation.', [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Remove', style: 'destructive', onPress: () => run(() => removeParticipant(projectId, conversationId, participant.id)) },
    ]);
  };

  const attachedAgentIds = new Set(
    participants.filter((p) => p.entity_name === 'application' || p.entity_name === 'pipeline').map((p) => Number(p.entity_meta?.id)),
  );

  const title = mode === 'people' ? 'Add people' : mode === 'agents' ? 'Add agent' : 'Participants';
  const back = () => {
    setMode('list');
    setQuery('');
  };

  return (
    <View style={[styles.sheet, { backgroundColor: colors.elevated, paddingTop: Math.max(insets.top, 12) }]}>
      <View style={styles.header}>
        {mode !== 'list' ? <Button title="Back" icon="back" variant="plain" size="small" onPress={back} /> : null}
        <Text variant="title3" heading style={styles.flex} numberOfLines={1}>
          {title}
        </Text>
        <Button title="Done" variant="plain" size="small" onPress={onClose} />
      </View>
      {offline ? (
        <View style={[styles.note, { backgroundColor: colors.warningSoft, borderRadius: radius.md }]}>
          <Icon name="offline" size={16} tone="warning" />
          <Text variant="footnote" tone="warning" style={styles.flex}>
            You&apos;re offline.
          </Text>
        </View>
      ) : null}
      {error ? (
        <View accessible accessibilityRole="alert" style={[styles.note, { backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={16} tone="danger" />
          <Text variant="footnote" tone="danger" style={styles.flex}>
            {error}
          </Text>
        </View>
      ) : null}
      {busy ? <ActivityIndicator color={colors.accent} /> : null}

      {mode === 'list' ? (
        <ScrollView contentContainerStyle={[styles.list, { paddingBottom: Math.max(insets.bottom, 16) }]}>
          <ListSection>
            {participants.map((p) => {
              const name = participantName(p) ?? KIND_LABEL[p.entity_name] ?? p.entity_name;
              const me = p.entity_name === 'user' && participantUserId(p) === selfUserId;
              return (
                <ListRow
                  key={p.id}
                  title={me ? `${name} (you)` : name}
                  subtitle={KIND_LABEL[p.entity_name] ?? p.entity_name}
                  leading={<Avatar name={name} kind={p.entity_name === 'user' ? 'person' : 'agent'} size={36} decorative />}
                  trailing={
                    isRemovable(p, selfUserId) ? (
                      <Button
                        title="Remove"
                        variant="destructivePlain"
                        size="small"
                        accessibilityLabel={`Remove ${participantName(p) ?? 'participant'}`}
                        disabled={offline || busy}
                        onPress={() => confirmRemove(p)}
                      />
                    ) : undefined
                  }
                />
              );
            })}
          </ListSection>
          <ListSection>
            {peopleAvailable ? <ListRow icon="personAdd" title="Add people" disabled={offline} onPress={() => setMode('people')} /> : null}
            {canAddAgent ? (
              <ListRow icon="agent" title="Add agent" disabled={offline} onPress={() => setMode('agents')} />
            ) : (
              <ListRow icon="info" title="Agents can't join a conversation with toolkits." />
            )}
          </ListSection>
        </ScrollView>
      ) : (
        <>
          <View style={[styles.search, { backgroundColor: colors.surfaceSunken, borderColor: colors.separatorStrong, borderRadius: radius.md }]}>
            <Icon name="search" size={18} tone="secondary" />
            <TextInput
              accessibilityLabel={mode === 'people' ? 'Search people' : 'Search agents'}
              placeholder="Search"
              placeholderTextColor={colors.textTertiary}
              autoFocus
              value={query}
              onChangeText={setQuery}
              clearButtonMode="while-editing"
              returnKeyType="search"
              maxFontSizeMultiplier={maxScale.body}
              style={[ramp.body, styles.flex, { color: colors.textPrimary, minHeight: hitTarget() }]}
            />
          </View>
          <ScrollView keyboardShouldPersistTaps="handled" contentContainerStyle={[styles.list, { paddingBottom: Math.max(insets.bottom, 16) }]}>
            <ListSection>
              {mode === 'people'
                ? (candidates.data?.rows ?? []).map((c) => (
                    <ListRow
                      key={c.userId}
                      title={c.name}
                      subtitle={c.email || null}
                      leading={<Avatar name={c.name} size={36} decorative />}
                      value={c.alreadyParticipant ? 'Already here' : null}
                      trailing={c.alreadyParticipant ? undefined : <Icon name="plus" size={20} tone="accent" />}
                      accessibilityLabel={c.alreadyParticipant ? `${c.name}, already here` : `Add ${c.name}`}
                      disabled={c.alreadyParticipant || busy}
                      chevron={false}
                      onPress={() => run(async () => void (await addPersonParticipant(projectId, conversationId, c)))}
                    />
                  ))
                : (agents.data ?? []).map((a) => {
                    const here = attachedAgentIds.has(a.id);
                    return (
                      <ListRow
                        key={a.id}
                        title={a.name}
                        subtitle={a.description || null}
                        leading={<Avatar name={a.name} kind="agent" size={36} decorative />}
                        value={here ? 'Already here' : null}
                        trailing={here ? undefined : <Icon name="plus" size={20} tone="accent" />}
                        accessibilityLabel={here ? `${a.name}, already here` : `Add ${a.name}`}
                        disabled={here || busy}
                        chevron={false}
                        onPress={() =>
                          run(async () => {
                            const row = await addAgentParticipant(projectId, conversationId, a);
                            onAgentAdded?.(row.id);
                          })
                        }
                      />
                    );
                  })}
            </ListSection>
            {(mode === 'people' ? candidates.isLoading : agents.isLoading) ? <ActivityIndicator color={colors.accent} /> : null}
          </ScrollView>
        </>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { flex: 1, paddingHorizontal: 16, gap: 12 },
  header: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  flex: { flex: 1 },
  note: { flexDirection: 'row', alignItems: 'center', gap: 8, padding: 10, borderCurve: 'continuous' },
  list: { gap: 20 },
  search: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, borderWidth: 1, borderCurve: 'continuous' },
});
