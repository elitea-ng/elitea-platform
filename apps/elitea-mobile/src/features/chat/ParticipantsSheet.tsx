import { useState } from 'react';
import { ActivityIndicator, Alert, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';

import { useAgents } from '@/api/queries/agents';
import {
  addAgentParticipant,
  addPersonParticipant,
  participantErrorText,
  removeParticipant,
  useParticipantCandidates,
  type Participant,
} from '@/api/queries/participants';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useModalAnimation } from '@/features/a11y/motion';
import { participantName, participantUserId } from '@/features/chat/messageModel';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { ramp } from '@/ui';

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

function SheetBody({ onClose, projectId, conversationId, participants, selfUserId, offline, canAddAgent = true, onChanged, onAgentAdded }: ParticipantsSheetProps) {
  const palette = usePalette();
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

  return (
    <View style={[styles.sheet, { backgroundColor: palette.background }]}>
      <View style={styles.header}>
        {mode !== 'list' ? (
          <Pressable accessibilityRole="button" onPress={() => { setMode('list'); setQuery(''); }} hitSlop={12}>
            <Text style={[styles.action, { color: palette.accent }]}>Back</Text>
          </Pressable>
        ) : null}
        <Text accessibilityRole="header" style={[styles.title, { color: palette.text }]}>
          {mode === 'people' ? 'Add people' : mode === 'agents' ? 'Add agent' : 'Participants'}
        </Text>
        <Pressable accessibilityRole="button" onPress={onClose} hitSlop={12}>
          <Text style={[styles.action, { color: palette.accent }]}>Done</Text>
        </Pressable>
      </View>
      {offline ? <Text style={[styles.note, { color: palette.textSecondary }]}>You&apos;re offline.</Text> : null}
      {error ? (
        <Text accessibilityRole="alert" style={[styles.note, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      {busy ? <ActivityIndicator color={palette.accent} /> : null}

      {mode === 'list' ? (
        <ScrollView contentContainerStyle={styles.list}>
          {participants.map((p) => (
            <View key={p.id} style={[styles.row, { borderColor: palette.border }]}>
              <View style={styles.texts}>
                <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
                  {participantName(p) ?? KIND_LABEL[p.entity_name] ?? p.entity_name}
                  {p.entity_name === 'user' && participantUserId(p) === selfUserId ? ' (you)' : ''}
                </Text>
                <Text style={[styles.kind, { color: palette.textSecondary }]}>{KIND_LABEL[p.entity_name] ?? p.entity_name}</Text>
              </View>
              {isRemovable(p, selfUserId) ? (
                <Pressable
                  accessibilityRole="button"
                  accessibilityLabel={`Remove ${participantName(p) ?? 'participant'}`}
                  accessibilityState={{ disabled: offline || busy }}
                  disabled={offline || busy}
                  onPress={() => confirmRemove(p)}>
                  <Text style={[styles.action, { color: palette.danger, opacity: offline ? 0.4 : 1 }]}>Remove</Text>
                </Pressable>
              ) : null}
            </View>
          ))}
          <View style={styles.adds}>
            {peopleAvailable ? (
              <Pressable accessibilityRole="button" disabled={offline} onPress={() => setMode('people')}>
                <Text style={[styles.action, { color: palette.accent, opacity: offline ? 0.4 : 1 }]}>Add people</Text>
              </Pressable>
            ) : null}
            {canAddAgent ? (
              <Pressable accessibilityRole="button" disabled={offline} onPress={() => setMode('agents')}>
                <Text style={[styles.action, { color: palette.accent, opacity: offline ? 0.4 : 1 }]}>Add agent</Text>
              </Pressable>
            ) : (
              <Text style={[styles.note, { color: palette.textSecondary }]}>Agents can&apos;t join a conversation with toolkits.</Text>
            )}
          </View>
        </ScrollView>
      ) : (
        <>
          <TextInput
            accessibilityLabel={mode === 'people' ? 'Search people' : 'Search agents'}
            placeholder="Search"
            placeholderTextColor={palette.textSecondary}
            autoFocus
            value={query}
            onChangeText={setQuery}
            style={[styles.search, { color: palette.text, borderColor: palette.inputBorder }]}
          />
          <ScrollView keyboardShouldPersistTaps="handled" contentContainerStyle={styles.list}>
            {mode === 'people'
              ? (candidates.data?.rows ?? []).map((c) => (
                  <Pressable
                    key={c.userId}
                    accessibilityRole="button"
                    accessibilityLabel={c.alreadyParticipant ? `${c.name}, already here` : `Add ${c.name}`}
                    disabled={c.alreadyParticipant || busy}
                    onPress={() => run(async () => void (await addPersonParticipant(projectId, conversationId, c)))}
                    style={[styles.row, { borderColor: palette.border }]}>
                    <View style={styles.texts}>
                      <Text style={[styles.name, { color: palette.text }]}>{c.name}</Text>
                      {c.email ? <Text style={[styles.kind, { color: palette.textSecondary }]}>{c.email}</Text> : null}
                    </View>
                    <Text style={[styles.kind, { color: palette.textSecondary }]}>{c.alreadyParticipant ? 'Already here' : 'Add'}</Text>
                  </Pressable>
                ))
              : (agents.data ?? []).map((a) => (
                  <Pressable
                    key={a.id}
                    accessibilityRole="button"
                    accessibilityLabel={attachedAgentIds.has(a.id) ? `${a.name}, already here` : `Add ${a.name}`}
                    disabled={attachedAgentIds.has(a.id) || busy}
                    onPress={() =>
                      run(async () => {
                        const row = await addAgentParticipant(projectId, conversationId, a);
                        onAgentAdded?.(row.id);
                      })
                    }
                    style={[styles.row, { borderColor: palette.border }]}>
                    <View style={styles.texts}>
                      <Text style={[styles.name, { color: palette.text }]}>{a.name}</Text>
                      {a.description ? (
                        <Text numberOfLines={1} style={[styles.kind, { color: palette.textSecondary }]}>
                          {a.description}
                        </Text>
                      ) : null}
                    </View>
                    <Text style={[styles.kind, { color: palette.textSecondary }]}>{attachedAgentIds.has(a.id) ? 'Already here' : 'Add'}</Text>
                  </Pressable>
                ))}
            {(mode === 'people' ? candidates.isLoading : agents.isLoading) ? <ActivityIndicator color={palette.accent} /> : null}
          </ScrollView>
        </>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { flex: 1, padding: Spacing.three, gap: Spacing.two },
  header: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two },
  title: { ...ramp.headline, flex: 1, fontWeight: '700' },
  action: { ...ramp.headline, fontWeight: '600' },
  note: { ...ramp.subheadline },
  list: { gap: 0, paddingBottom: Spacing.four },
  row: { flexDirection: 'row', alignItems: 'center', paddingVertical: Spacing.two + 2, borderBottomWidth: StyleSheet.hairlineWidth, gap: Spacing.two },
  texts: { flex: 1 },
  name: { ...ramp.body },
  kind: { ...ramp.footnote },
  adds: { flexDirection: 'row', gap: Spacing.four, paddingTop: Spacing.three },
  search: { ...ramp.body, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, paddingHorizontal: Spacing.two, paddingVertical: Spacing.two },
});
