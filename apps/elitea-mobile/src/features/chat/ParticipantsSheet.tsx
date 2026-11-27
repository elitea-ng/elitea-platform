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
import { setConversationLead } from '@/api/queries/conversationActions';
import {
  CONVERSATION_SKILL_LIMIT,
  pinConversationSkill,
  skillPinErrorText,
  unpinConversationSkill,
  useConversationSkills,
  useProjectSkills,
} from '@/api/queries/conversationSkills';
import { useDesign } from '@/design/DesignProvider';
import { TURN_TAKING_COPY } from '@/features/chat/turnTakingCopy';
import { floorOf } from '@elitea/client-core/conversation';
import { agentExecutorOf } from '@/features/localWork/model';
import { TEAM_COPY } from '@/features/localWork/teamCopy';
import { useModalAnimation } from '@/features/a11y/motion';
import { participantAgentId, participantIconPath, participantName, participantUserId } from '@/features/chat/messageModel';
import { PackAvatar, useAgentIconUri } from '@/features/chat/AssistantAvatar';
import { participantAvatarPack } from '@/features/chat/avatarPackMedia';
import { ProbationBadge } from '@/features/workforce/ProbationBadge';
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
  /**
   * The conversation's meta (its `floor`, ADR-0050 7d). When given, the lead agent wears a
   * "Lead" badge and each agent row offers "Make lead" / "Clear lead" (an updateConversation).
   */
  conversationMeta?: unknown;
  /**
   * Opens what an agent participant remembers (client contract 1.44, ADR-0050 1b). When given,
   * each agent row offers "What it remembers"; the caller closes the sheet and navigates.
   */
  onAgentMemory?: (agentId: number, name: string) => void;
  /** Opens the agent's delegation grants (client contract 1.59). When given, each agent row offers "Delegations". */
  onAgentDelegations?: (agentId: number, name: string) => void;
  /**
   * A Local work thread viewed here (ADR-0061 Parity, #3069): the team is read-only. Each agent
   * row says where it runs (Local or Cloud) and the lead keeps its badge, but the lead cannot be
   * changed and nobody is added or removed: the desktop owns the thread.
   */
  teamView?: boolean;
}

/** Where an agent of a team thread runs: a still badge, "Local" or "Cloud" (ADR-0061 Parity). */
function ExecutorBadge({ name, cloud, participantId }: { name: string; cloud: boolean; participantId: number }) {
  const { colors, radius } = useDesign();
  return (
    <View
      accessible
      accessibilityLabel={TEAM_COPY.executorLabel(name, cloud)}
      testID={`executor-badge-${participantId}`}
      style={[styles.badge, { backgroundColor: cloud ? colors.accentSoft : colors.surfaceSunken, borderRadius: radius.pill }]}>
      <Text variant="caption" tone={cloud ? 'accent' : 'secondary'}>
        {cloud ? TEAM_COPY.executorCloud : TEAM_COPY.executorLocal}
      </Text>
    </View>
  );
}

/**
 * An agent row's avatar place (#3131, #3130): its avatar pack's still, else its logo (`icon_meta`,
 * read from the participant as web and desktop read it), else its monogram. The logo is downloaded
 * only when there is no pack (the pack's still wins).
 */
function AgentRowAvatar({ projectId, participant, name }: { projectId: number; participant: Participant; name: string }) {
  const pack = participantAvatarPack(participant);
  const icon = useAgentIconUri(projectId, pack === undefined ? participantIconPath(participant) : null);
  return <PackAvatar projectId={projectId} pack={pack} motion="still" name={name} imageUri={icon} mark={false} size={36} />;
}

/** Agents and pipelines: the participants a lead can be. */
export function canLead(participant: Participant): boolean {
  return participant.entity_name === 'application' || participant.entity_name === 'pipeline';
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

type Mode = 'list' | 'people' | 'agents' | 'skills';

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
  conversationMeta,
  onAgentMemory,
  onAgentDelegations,
  teamView = false,
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
  // ADR-0050 amendment 1, A1.3: the skills pinned to the conversation, in their own section below the
  // participants. null = this workspace has none (before client contract 1.57), so the section is left out.
  const pins = useConversationSkills(projectId, conversationId, !offline);
  const skillOptions = useProjectSkills(projectId, mode === 'skills' && !offline);
  const pinList = pins.data ?? null;
  const pinnedSkillIds = new Set((pinList ?? []).flatMap((pin) => (pin.skill_id === undefined ? [] : [pin.skill_id])));
  const runSkills = (work: () => Promise<unknown>) => {
    if (busy || offline) return;
    setBusy(true);
    setError(null);
    work()
      .then(() => pins.refetch())
      .then(() => {
        setMode('list');
        setQuery('');
      })
      .catch((e: unknown) => setError(skillPinErrorText(e)))
      .finally(() => setBusy(false));
  };

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

  const leadId = floorOf(conversationMeta).leadParticipantId;
  const leadsShown = conversationMeta !== undefined && Number.isSafeInteger(Number(conversationId));
  const agentCount = participants.filter(canLead).length;
  const toggleLead = (participant: Participant) => {
    if (busy || offline) return;
    setBusy(true);
    setError(null);
    setConversationLead(projectId, Number(conversationId), conversationMeta, participant.id === leadId ? 0 : participant.id)
      .then(onChanged)
      .catch(() => setError(TURN_TAKING_COPY.leadFailed))
      .finally(() => setBusy(false));
  };

  const attachedAgentIds = new Set(
    participants.filter((p) => p.entity_name === 'application' || p.entity_name === 'pipeline').map((p) => Number(p.entity_meta?.id)),
  );

  const title = mode === 'people' ? 'Add people' : mode === 'agents' ? 'Add agent' : mode === 'skills' ? 'Add skill' : 'Participants';
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
              const isLead = leadsShown && p.id === leadId;
              const leadable = !teamView && leadsShown && canLead(p) && (agentCount > 1 || isLead);
              const removable = !teamView && isRemovable(p, selfUserId);
              const executor = teamView && canLead(p) ? agentExecutorOf(p) : null;
              const baseKind = KIND_LABEL[p.entity_name] ?? p.entity_name;
              const kind = executor === null ? baseKind : `${baseKind} · ${executor === 'cloud' ? TEAM_COPY.executorCloud : TEAM_COPY.executorLocal}`;
              const agentId = onAgentMemory ? participantAgentId(p) : null;
              const remembers = agentId !== null && onAgentMemory ? () => onAgentMemory(agentId, name) : undefined;
              const delegatesId = onAgentDelegations ? participantAgentId(p) : null;
              const delegations = delegatesId !== null && onAgentDelegations ? () => onAgentDelegations(delegatesId, name) : undefined;
              return (
                <ListRow
                  key={p.id}
                  title={me ? `${name} (you)` : name}
                  subtitle={isLead ? `${kind} · ${TURN_TAKING_COPY.lead}` : kind}
                  leading={
                    p.entity_name === 'user' ? (
                      <Avatar name={name} kind="person" size={36} decorative />
                    ) : (
                      <AgentRowAvatar projectId={projectId} participant={p} name={name} />
                    )
                  }
                  trailing={
                    leadable || removable || executor !== null || remembers || delegations || (!offline && participantAgentId(p) !== null) ? (
                      <View style={styles.trailing}>
                        {executor !== null ? <ExecutorBadge name={name} cloud={executor === 'cloud'} participantId={p.id} /> : null}
                        {/* Draws only for an agent hired automatically and not kept yet (client contract 1.56). */}
                        {!offline ? <ProbationBadge projectId={projectId} agentId={participantAgentId(p)} /> : null}
                        {remembers ? (
                          <Button
                            title="What it remembers"
                            variant="plain"
                            size="small"
                            accessibilityLabel={`What ${name} remembers`}
                            disabled={offline}
                            onPress={remembers}
                          />
                        ) : null}
                        {delegations ? (
                          <Button
                            title="Delegations"
                            variant="plain"
                            size="small"
                            accessibilityLabel={`${name} delegations`}
                            disabled={offline}
                            onPress={delegations}
                          />
                        ) : null}
                        {leadable ? (
                          <Button
                            title={isLead ? TURN_TAKING_COPY.clearLead : TURN_TAKING_COPY.makeLead}
                            variant="plain"
                            size="small"
                            accessibilityLabel={isLead ? TURN_TAKING_COPY.clearLeadLabel(name) : TURN_TAKING_COPY.makeLeadLabel(name)}
                            disabled={offline || busy}
                            onPress={() => toggleLead(p)}
                          />
                        ) : null}
                        {removable ? (
                      <Button
                        title="Remove"
                        variant="destructivePlain"
                        size="small"
                        accessibilityLabel={`Remove ${participantName(p) ?? 'participant'}`}
                        disabled={offline || busy}
                        onPress={() => confirmRemove(p)}
                      />
                        ) : null}
                      </View>
                    ) : undefined
                  }
                />
              );
            })}
          </ListSection>
          {teamView ? null : (
          <ListSection>
            {peopleAvailable ? <ListRow icon="personAdd" title="Add people" disabled={offline} onPress={() => setMode('people')} /> : null}
            {canAddAgent ? (
              <ListRow icon="agent" title="Add agent" disabled={offline} onPress={() => setMode('agents')} />
            ) : (
              <ListRow icon="info" title="Agents can't join a conversation with toolkits." />
            )}
          </ListSection>
          )}
          {pinList !== null ? (
            <ListSection
              title={`Skills (${pinList.length})`}
              footer="Applied to every reply here, whichever agent or model answers."
            >
              {pinList.map((pin) => {
                const name = pin.name === '' ? 'A skill that is no longer available' : pin.name;
                const untrusted = pin.trust.state === 'untrusted';
                const origin = pin.kind === 'listing' ? `Marketplace ${pin.version_name ?? ''}`.trim() : 'Project skill';
                const note = pin.available ? '' : ' · Not applied for you: not available to you';
                return (
                  <ListRow
                    key={pin.id}
                    title={name}
                    subtitle={`${untrusted ? `${origin} · Untrusted: the model gets its text as quoted data` : origin}${note}`}
                    trailing={
                      <Button
                        title="Remove"
                        variant="destructivePlain"
                        size="small"
                        accessibilityLabel={`Remove ${name} from this conversation`}
                        disabled={offline || busy}
                        onPress={() => runSkills(() => unpinConversationSkill(projectId, conversationId, pin.id))}
                      />
                    }
                  />
                );
              })}
              {pinList.length < CONVERSATION_SKILL_LIMIT ? (
                <ListRow icon="plus" title="Add skill" disabled={offline} onPress={() => setMode('skills')} />
              ) : (
                <ListRow icon="info" title="This conversation holds five skills." />
              )}
            </ListSection>
          ) : null}
        </ScrollView>
      ) : (
        <>
          <View style={[styles.search, { backgroundColor: colors.surfaceSunken, borderColor: colors.separatorStrong, borderRadius: radius.md }]}>
            <Icon name="search" size={18} tone="secondary" />
            <TextInput
              accessibilityLabel={mode === 'people' ? 'Search people' : mode === 'skills' ? 'Search skills' : 'Search agents'}
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
              {mode === 'skills'
                ? (skillOptions.data ?? [])
                    .filter((skill) => !pinnedSkillIds.has(skill.id) && skill.name.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()))
                    .map((skill) => (
                      <ListRow
                        key={skill.id}
                        title={skill.name}
                        subtitle={skill.description || null}
                        value={skill.untrusted ? 'Untrusted' : null}
                        trailing={<Icon name="plus" size={20} tone="accent" />}
                        accessibilityLabel={`Add ${skill.name}`}
                        disabled={busy}
                        chevron={false}
                        onPress={() => runSkills(() => pinConversationSkill(projectId, conversationId, skill.id))}
                      />
                    ))
                : mode === 'people'
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
            {(mode === 'people' ? candidates.isLoading : mode === 'skills' ? skillOptions.isLoading : agents.isLoading) ? <ActivityIndicator color={colors.accent} /> : null}
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
  trailing: { flexDirection: 'row', alignItems: 'center', gap: 4 },
  badge: { paddingHorizontal: 8, paddingVertical: 2 },
  note: { flexDirection: 'row', alignItems: 'center', gap: 8, padding: 10, borderCurve: 'continuous' },
  list: { gap: 20 },
  search: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, borderWidth: 1, borderCurve: 'continuous' },
});
