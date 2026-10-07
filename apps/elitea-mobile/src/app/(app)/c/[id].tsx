import { Stack, router, useLocalSearchParams } from 'expo-router';
import * as Linking from 'expo-linking';
import { useEffect, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Alert,
  FlatList,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';

import { isConversationRef, useConversation } from '@/api/queries/conversation';
import { useModels } from '@/api/queries/models';
import type { TurnModel } from '@/api/queries/sendMessage';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { Composer } from '@/features/chat/Composer';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { speaksContract } from '@/api/discovery';
import { restorePreparedTurn, TurnInputError } from '@/api/queries/sendMessage';
import type { ActiveTrigger, MentionCandidate, TriggerChar } from '@/features/chat/mentions/mentions';
import {
  chatTarget,
  agentOptions,
  agentsCanJoin,
  participantName,
  participantUserId,
  toChatMessages,
  type ChatAttachment,
  type ChatMessage,
} from '@/features/chat/messageModel';
import type { SendExtras } from '@/features/chat/useTurn';
import { useMessageTraces } from '@/api/queries/messageTraces';
import { ToolActivity } from '@/features/chat/ToolCalls';
import { ToolDetailSheet } from '@/features/chat/ToolDetailSheet';
import { markPausedSteps, type ToolStep, type TurnPause } from '@/features/chat/toolActivity';
import { PauseCard } from '@/features/chat/pauses/PauseCard';
import { useQueryClient } from '@tanstack/react-query';
import { useAgents, useAgentVersion } from '@/api/queries/agents';
import { AttachButton, AttachmentTrayView, type PickSource } from '@/features/chat/attachments/AttachmentTrayView';
import { attachmentGate } from '@/features/chat/attachments/gate';
import { PickerPermissionError, pickFiles, pickPhotos, takePhoto } from '@/features/chat/attachments/pickers';
import { attachmentLimits } from '@/features/chat/attachments/prepare';
import { useAttachmentTray } from '@/features/chat/attachments/useAttachmentTray';
import { AttachmentStrip, ImageViewer } from '@/features/chat/attachments/AttachmentStrip';
import { useAttachmentOpener } from '@/features/chat/attachments/useAttachmentOpener';
import { queryKeys } from '@/api/queries/keys';
import { addAgentParticipant, participantErrorText } from '@/api/queries/participants';
import type { PickResult } from '@/features/chat/Composer';
import { ParticipantsSheet } from '@/features/chat/ParticipantsSheet';
import type { Suggestion, SuggestionState } from '@/features/chat/SuggestionStrip';
import { TargetChip } from '@/features/chat/TargetChip';
import { setTarget, targetKey, useTarget } from '@/features/chat/targetStore';
import { useAgentSkills, useToolkitTools } from '@/api/queries/skillsTools';
import {
  matches,
  skillInsert,
  slashPhase,
  toolInsert,
  toolkitInsert,
  type ToolkitOption,
} from '@/features/chat/slashPicker';
import { pauseFromGroup } from '@/features/chat/pauses/continueBodies';
import { appActivity } from '@/lib/appActivity';
import { network, useOnline } from '@/lib/network';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { MessageActionsSheet } from '@/features/chat/actions/MessageActionsSheet';
import { planActions } from '@/features/chat/actions/messageActions';
import { usePolicy } from '@/security/policy';
import { useTurnAnnouncements } from '@/features/a11y/turnAnnouncements';
import { canDictateOnDevice } from '@/features/speech/dictation';
import { canReadAloud, readAloud, stopReading, useReading } from '@/features/speech/readAloud';
import { exportErrorText } from '@/api/queries/exportConversation';
import { shareTranscript } from '@/features/chat/export/shareTranscript';
import { answerHiddenByRegeneration, type LiveTurn } from '@/features/chat/turnController';
import { ATTACHMENT_ONLY_TEXT, useTurn } from '@/features/chat/useTurn';
import { Markdown } from '@/features/markdown/Markdown';
import { useSelfUserId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';
import { outboxDrainer, useOutboxItems } from '@/sync/appOutbox';
import { isOfflineCopy } from '@/sync/offlineReads';
import { outboxStatus, type OutboxItem } from '@/sync/outbox';

type Row =
  | { kind: 'persisted'; key: string; message: ChatMessage }
  | { kind: 'outbox'; key: string; item: OutboxItem }
  | { kind: 'live_user'; key: string; text: string; files: string[] }
  | { kind: 'live_reply'; key: string; live: LiveTurn };

/** One conversation: its latest messages, newest at the bottom, and the composer. */
export default function ThreadScreen() {
  const palette = usePalette();
  const { id } = useLocalSearchParams<{ id: string }>();
  const ref = typeof id === 'string' ? id : '';
  const projectId = useSession((s) => s.projectId);
  const conversation = useConversation(projectId, ref);
  const detail = conversation.data;
  // "Me" (the "who am I" read, made once per session by the app shell).
  const me = useSelfUserId();
  const messages = toChatMessages(detail, { selfUserId: me });
  const workspace = useWorkspaceInfo();
  const contract11 = speaksContract(workspace?.discovery, 1, 1);
  // Client contract 1.3: feedback, server stop, export, suggested agents.
  const contract13 = speaksContract(workspace?.discovery, 1, 3);
  const { policy } = usePolicy();
  const [actionsFor, setActionsFor] = useState<ChatMessage | null>(null);
  // On-device speech: dictation into the composer, and reading an answer aloud.
  const [dictation] = useState(canDictateOnDevice);
  const reading = useReading();
  useEffect(() => () => stopReading(), []);
  // The people "@" offers: every other person in the conversation.
  const people = useMemo<MentionCandidate[]>(() => {
    const out: MentionCandidate[] = [];
    for (const p of detail?.participants ?? []) {
      const userId = participantUserId(p);
      const name = participantName(p);
      if (userId === null || userId === me || !name || out.some((c) => c.userId === userId)) continue;
      out.push({ userId, name });
    }
    return out;
  }, [detail?.participants, me]);
  const queryClient = useQueryClient();
  const chosenKey = projectId !== null ? targetKey(projectId, ref) : null;
  const chosen = useTarget(chosenKey);
  const lastAnsweredBy = [...messages].reverse().find((m) => m.role === 'assistant')?.authorParticipantId ?? null;
  const target = chatTarget(detail, { targetParticipantId: chosen, lastAnsweredBy });
  // A model conversation with no saved model runs on the project's default.
  const needsDefaultModel = target.ok && target.kind === 'model' && target.model === null;
  const models = useModels(projectId, needsDefaultModel);
  const savedName = target.ok && target.kind === 'model' ? target.model?.name : undefined;
  const savedProject = target.ok && target.kind === 'model' ? target.model?.projectId : undefined;
  const fallback = models.data?.defaultModel ?? null;
  // The React Compiler memoises this; a manual useMemo can no longer be preserved here.
  const model: TurnModel | null =
    savedName !== undefined
      ? { name: savedName, projectId: savedProject ?? null }
      : needsDefaultModel && fallback
        ? { name: fallback.name, projectId: fallback.projectId }
        : null;
  const conversationUuid = typeof detail?.uuid === 'string' && detail.uuid !== '' ? detail.uuid : null;
  const addressable = target.ok && (target.kind === 'agent' || model !== null);
  const turn = useTurn({
    projectId,
    conversationRef: ref,
    conversationUuid,
    participantId: target.ok && addressable ? target.participantId : null,
    model: target.ok && target.kind === 'model' ? model : null,
  });
  const { live, dismiss } = turn;
  // VoiceOver hears when the answer is done, stopped, failed or waits for the user.
  useTurnAnnouncements(live, target.ok ? target.name : null);
  const highlight = {
    people: [...people.map((p) => p.name), ...(detail?.participants ?? []).filter((p) => p?.entity_name === 'user').map((p) => participantName(p) ?? '').filter(Boolean)],
    agents: agentOptions(detail).map((a) => a.name ?? '').filter(Boolean),
  };
  const [openStep, setOpenStep] = useState<ToolStep | null>(null);
  const online = useOnline();
  // What the newest answer waits for: the live turn's pause, else (after a
  // reload) the one its persisted metadata records.
  const newest = messages[messages.length - 1];
  const persistedPause =
    live && !live.settled
      ? null
      : newest?.role === 'assistant'
        ? pauseFromGroup(
            (detail?.message_groups ?? []).find(
              (g) => (g as Record<string, unknown>)?.uuid === newest.uuid && newest.uuid !== null,
            ) as Record<string, unknown> | undefined,
          )
        : null;
  const pause: TurnPause | null = live && !live.settled && live.turn.pause ? live.turn.pause : persistedPause;
  const pauseTarget =
    pause && projectId !== null && conversationUuid && (pause.messageId ?? live?.execution?.responseMessageId)
      ? {
          projectId,
          conversationUuid,
          messageId: (pause.messageId ?? live?.execution?.responseMessageId) as string,
          threadId: pause.threadId,
        }
      : null;
  const webUrl = workspace && detail?.id ? `${workspace.origin}/chat/${encodeURIComponent(String(detail.id))}` : null;
  // Back from the web app (a sign-in done there): read the transcript again.
  useEffect(() => {
    if (!pause) return;
    const sub = appActivity.addEventListener('change', (state) => {
      if (state === 'active' && network.isOnline()) void conversation.refetch();
    });
    return () => sub.remove();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pause?.kind, pause?.messageId]);
  // Tool history of the persisted answers (1.1), fetched for the loaded groups.
  const numericId = Number(detail?.id);
  // A fresh array each render is fine: the query is keyed by the ids, not the array.
  const answerGroups = messages.filter((m) => m.role === 'assistant' && m.groupId !== null).map((m) => m.groupId as number);
  const traces = useMessageTraces(
    projectId,
    Number.isSafeInteger(numericId) && numericId > 0 ? numericId : null,
    answerGroups,
    contract11 && !isOfflineCopy(detail),
  );
  // Before client contract 1.1 a model (adhoc) chat dropped mentions silently.
  const mentionHint =
    people.length > 0 && target.ok && target.kind === 'model' && !contract11 ? 'Mentions notify only in agent chats.' : null;
  const outbox = useOutboxItems(projectId, conversationUuid);

  // A note above the composer (a regeneration that could not start, or did not land).
  const [threadNote, setThreadNote] = useState<string | null>(null);
  // Once the persisted copy is back from the server, the live bubbles go.
  useEffect(() => {
    if (live?.settled) dismiss();
  }, [live?.settled, dismiss]);

  // Regenerate: the newest message, a persisted answer, with its question.
  const newestAnswer = newest?.role === 'assistant' && newest.uuid !== null && !newest.isStreaming ? newest : null;
  const regenerateQuestion = newestAnswer
    ? (messages.find((m) => m.role === 'user' && newestAnswer.replyToGroupId !== null && m.groupId === newestAnswer.replyToGroupId) ??
      [...messages].reverse().find((m) => m.role === 'user' && (m.createdAt ?? 0) <= (newestAnswer.createdAt ?? Infinity)) ??
      null)
    : null;
  const canRegenerate = (message: ChatMessage | null): boolean =>
    message !== null &&
    newestAnswer !== null &&
    message.key === newestAnswer.key &&
    regenerateQuestion?.uuid != null &&
    contract11 &&
    online &&
    !isOfflineCopy(detail) &&
    !turn.busy &&
    pause === null &&
    target.ok &&
    conversationUuid !== null;
  const regenerate = async (message: ChatMessage) => {
    if (!canRegenerate(message) || !regenerateQuestion?.uuid || !message.uuid) return;
    setThreadNote(null);
    const problem = await turn.regenerate({
      answerUuid: message.uuid,
      questionUuid: regenerateQuestion.uuid,
      questionText: regenerateQuestion.text,
      participantId: message.authorParticipantId ?? 0,
      // Proven once the transcript is read again: the rewritten answer must
      // carry the regeneration's id as its generation.
      // Unknown (the transcript could not be read again): say nothing.
      onSettled: (replaced) => setThreadNote(replaced === false ? "The workspace didn't replace this answer. Try again." : null),
    });
    if (problem !== null) setThreadNote(problem);
  };
  const actionPlan = planActions(actionsFor ?? { role: 'user', text: '', uuid: null, isStreaming: false }, {
    shareOut: policy.allow_share_out,
    contract13,
    online: online && !isOfflineCopy(detail),
    canSpeak: canReadAloud(),
    canRegenerate: canRegenerate(actionsFor),
  });

  // Attachments (contract 1.1 limits, else the server defaults): picked and
  // prepared now, uploaded on send. Online only; an agent must take files.
  const limits = attachmentLimits(workspace?.discovery.attachments);
  const { tray, items: trayItems } = useAttachmentTray(projectId, conversationUuid, limits);
  const targetAgent = target.ok && target.kind === 'agent' ? target.agent : null;
  const agentVersion = useAgentVersion(targetAgent ? (targetAgent.agentProjectId ?? projectId) : null, targetAgent?.agentId ?? null);
  const gateReason = attachmentGate({
    online,
    conversationUuid: isOfflineCopy(detail) ? null : conversationUuid,
    target: !target.ok
      ? { kind: 'none' }
      : target.kind === 'model'
        ? { kind: 'model' }
        : targetAgent?.agentId == null
          ? { kind: 'agent', versionStatus: 'error', internalTools: [] }
          : {
              kind: 'agent',
              versionStatus: agentVersion.isLoading ? 'loading' : agentVersion.data ? 'ready' : 'error',
              internalTools: agentVersion.data?.internalTools ?? [],
            },
  });
  // Files of sent messages: downloaded on demand (1.1 route), never kept.
  const opener = useAttachmentOpener(projectId, conversationUuid);
  const canOpenFiles = contract11 && online && !isOfflineCopy(detail);

  const pickInto = async (source: PickSource) => {
    try {
      const picked =
        source === 'photos' ? await pickPhotos(limits.maxFiles - tray.sendable) : source === 'camera' ? await takePhoto() : await pickFiles();
      if (picked.length === 0) return;
      const { refused, warnings } = tray.add(picked);
      const notes = [...refused.map((r) => r.message), ...warnings];
      if (notes.length > 0) Alert.alert(refused.length > 0 ? 'Some files were not attached' : 'Attached', notes.join('\n'));
    } catch (error) {
      if (error instanceof PickerPermissionError) {
        Alert.alert('Camera access', 'Allow camera access in Settings to take a photo here.');
        return;
      }
      Alert.alert('Attach files', "Those files couldn't be opened.");
    }
  };
  const sendWithFiles = async (text: string, mentions: SendExtras['mentions']): Promise<string | null> => {
    if (trayItems.length === 0) return turn.send(text, { mentions });
    if (gateReason) return gateReason;
    // Every file first: a message with files is sent (or queued) only once all are stored.
    const uploaded = await tray.uploadAll();
    if (!uploaded.ok) return uploaded.error;
    const problem = await turn.send(text.trim() === '' ? ATTACHMENT_ONLY_TEXT : text, { mentions, attachments: uploaded.attachments });
    if (problem === null) tray.clear(uploaded.ids);
    return problem;
  };

  // "#": find an agent, add it (pinned to its version) and address it (1.1, online).
  const conversationNumericId = Number.isSafeInteger(numericId) && numericId > 0 ? String(numericId) : null;
  const canManage = contract11 && online && conversationNumericId !== null && projectId !== null && !isOfflineCopy(detail);
  const [trigger, setTrigger] = useState<ActiveTrigger | null>(null);
  // Not next to toolkits: the server refuses a turn that mixes them.
  const canAddAgent = canManage && agentsCanJoin(detail);
  const agentSearch = useAgents(trigger?.trigger === '#' && canAddAgent ? projectId : null, trigger?.query ?? '');
  const attached = target.ok && target.kind === 'agent' ? target.choices : [];
  // "~": the addressed agent's skills; "/": a model chat's toolkits, then their tools (text only).
  const toolkits: ToolkitOption[] = (detail?.participants ?? [])
    .filter((p) => p?.entity_name === 'toolkit')
    .map((p) => ({ toolkitId: Number(p.entity_meta?.id), name: participantName(p) ?? '' }))
    .filter((t) => Number.isSafeInteger(t.toolkitId) && t.toolkitId > 0 && t.name !== '');
  const skillVersion = target.ok && target.kind === 'agent' ? target.agent.versionId : null;
  const offerSkills = contract11 && skillVersion !== null;
  const offerTools = contract11 && target.ok && target.kind === 'model' && toolkits.length > 0;
  const skills = useAgentSkills(projectId, skillVersion, trigger?.trigger === '~' && offerSkills);
  const slash = trigger?.trigger === '/' ? slashPhase(trigger.query, toolkits) : null;
  const tools = useToolkitTools(projectId, slash?.phase === 'tool' ? slash.toolkit.toolkitId : null, offerTools && slash?.phase === 'tool');
  const externalTriggers: TriggerChar[] = [
    ...(canAddAgent ? (['#'] as TriggerChar[]) : []),
    ...(offerSkills ? (['~'] as TriggerChar[]) : []),
    ...(offerTools ? (['/'] as TriggerChar[]) : []),
  ];
  const suggestionState = ((): SuggestionState | null => {
    switch (trigger?.trigger) {
      case '#':
        return {
          loading: agentSearch.isLoading,
          message: agentSearch.error ? "Couldn't load agents." : null,
          items: (agentSearch.data ?? []).slice(0, 20).map(
            (a): Suggestion => ({
              key: `agent-${a.id}`,
              title: a.name,
              subtitle: a.description || 'Agent',
              badge: attached.some((c) => c.agentId === a.id) ? 'Attached' : 'Add',
            }),
          ),
        };
      case '~':
        return {
          loading: skills.isLoading,
          message: skills.data === null || skills.error ? 'No skills to offer here.' : skills.data?.length === 0 ? 'This agent has no skills.' : null,
          items: (skills.data ?? [])
            .filter((sk) => matches(sk.name, trigger.query))
            .map((sk) => ({ key: `skill-${sk.name}`, title: sk.name, subtitle: sk.description || 'Skill', initial: '~' })),
        };
      case '/':
        if (slash?.phase === 'toolkit') {
          return {
            items: toolkits
              .filter((t) => matches(t.name, slash.filter))
              .map((t) => ({ key: `toolkit-${t.toolkitId}`, title: t.name, subtitle: 'Toolkit', initial: '/' })),
          };
        }
        if (slash?.phase === 'tool') {
          return {
            loading: tools.isLoading,
            message: tools.data === null || tools.error ? "This toolkit's tools could not be listed." : null,
            items: (tools.data ?? [])
              .filter((t) => matches(t.name, slash.filter))
              .slice(0, 30)
              .map((t) => ({ key: `tool-${t.name}`, title: t.name, subtitle: t.description || slash.toolkit.name, initial: '/' })),
          };
        }
        return { items: [], message: 'No toolkit by that name in this conversation.' };
      default:
        return null;
    }
  })();
  const pickExternal = async (picked: ActiveTrigger, item: Suggestion): Promise<PickResult> => {
    if (picked.trigger === '~') return { insert: skillInsert(item.title) };
    if (picked.trigger === '/') {
      const phase = slashPhase(picked.query, toolkits);
      if (phase.phase === 'toolkit') {
        const toolkit = toolkits.find((t) => `toolkit-${t.toolkitId}` === item.key);
        return toolkit ? { insert: toolkitInsert(toolkit) } : { error: 'That toolkit is no longer here.' };
      }
      if (phase.phase === 'tool') return { insert: toolInsert(phase.toolkit, item.title) };
      return { error: 'No toolkit by that name in this conversation.' };
    }
    if (picked.trigger !== '#' || !canAddAgent || projectId === null || conversationNumericId === null || chosenKey === null) {
      return { error: 'That cannot be added here.' };
    }
    const agent = (agentSearch.data ?? []).find((a) => `agent-${a.id}` === item.key);
    if (!agent) return { error: 'That agent is no longer listed.' };
    const already = attached.find((c) => c.agentId === agent.id);
    if (already) {
      setTarget(chosenKey, already.participantId);
      return { insert: '' };
    }
    try {
      const row = await addAgentParticipant(projectId, conversationNumericId, agent);
      setTarget(chosenKey, row.id);
      await queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, ref) });
      return { insert: '' };
    } catch (error) {
      return { error: participantErrorText(error) };
    }
  };
  const [peopleOpen, setPeopleOpen] = useState(false);
  // Export (1.3, allow_share_out): the whole transcript to the share sheet.
  const canExport = contract13 && policy.allow_share_out && online && !isOfflineCopy(detail) && conversationNumericId !== null && projectId !== null;
  const [exporting, setExporting] = useState(false);
  const exportTranscript = async () => {
    if (!canExport || exporting || projectId === null || conversationNumericId === null) return;
    setExporting(true);
    setThreadNote(null);
    try {
      await shareTranscript(projectId, conversationNumericId);
    } catch (error) {
      setThreadNote(exportErrorText(error));
    } finally {
      setExporting(false);
    }
  };

  if (!isConversationRef(ref)) return <StatusScreen title="Conversation not found" />;
  if (conversation.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: palette.background }]}>
        <ActivityIndicator color={palette.accent} />
      </View>
    );
  }
  if (conversation.error && !detail) {
    return (
      <StatusScreen title="Couldn't open this conversation" body="Check your connection and try again.">
        <Pressable accessibilityRole="button" onPress={() => void conversation.refetch()}>
          <Text style={[styles.action, { color: palette.accent }]}>Retry</Text>
        </Pressable>
      </StatusScreen>
    );
  }

  const rows: Row[] = [];
  // Inverted list: the first rows are the newest, at the bottom.
  for (const item of [...outbox].reverse()) rows.push({ kind: 'outbox', key: `outbox-${item.questionId}`, item });
  if (live && !live.settled) {
    rows.push({ kind: 'live_reply', key: `live-reply-${live.questionId}`, live });
    const lastUser = [...messages].reverse().find((m) => m.role === 'user');
    if (!live.continuation && lastUser?.text !== live.userText) {
      rows.push({ kind: 'live_user', key: `live-user-${live.questionId}`, text: live.userText, files: live.attachmentNames });
    }
  }
  // A regenerated answer is rewritten in place: its old text hides while the new one streams.
  const replacing = answerHiddenByRegeneration(live);
  for (const message of [...messages].reverse()) {
    if (replacing !== null && message.uuid === replacing) continue;
    rows.push({ kind: 'persisted', key: message.key, message });
  }

  const pauseBlocks = pause !== null && pause.kind !== 'output_limit' && !(pause.kind === 'hitl' && pause.shape === 'fanout');
  const disabledReason = pauseBlocks
    ? pause?.kind === 'auth'
      ? 'Skip the toolkit sign-in above, or continue in the web app.'
      : 'Waiting for your approval above.'
    : !conversationUuid
    ? 'This conversation cannot be messaged from mobile.'
    : !target.ok
      ? target.reason === 'several'
        ? 'This conversation has several models. Continue it in the web app.'
        : target.reason === 'toolkit_agent'
          ? 'This conversation mixes toolkits and agents. Continue it in the web app.'
          : 'This conversation has no agent or model you can message from mobile.'
      : addressable
        ? null
        : models.isLoading
          ? 'Loading the workspace model…'
          : 'This project has no model to chat with.';

  return (
    <KeyboardAvoidingView
      style={[styles.flex, { backgroundColor: palette.background }]}
      behavior={Platform.OS === 'ios' ? 'padding' : undefined}
      keyboardVerticalOffset={Platform.OS === 'ios' ? 100 : 0}>
      <Stack.Screen
        options={{
          title: detail?.name || 'Conversation',
          headerRight:
            (contract11 && conversationNumericId !== null) || canExport
              ? () => (
                  <View style={styles.headerActions}>
                    {canExport ? (
                      <Pressable
                        accessibilityRole="button"
                        accessibilityLabel="Export conversation"
                        accessibilityState={{ busy: exporting, disabled: exporting }}
                        disabled={exporting}
                        onPress={() => void exportTranscript()}
                        hitSlop={10}>
                        <Text maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.action, { color: palette.accent, opacity: exporting ? 0.5 : 1 }]}>
                          Export
                        </Text>
                      </Pressable>
                    ) : null}
                    {contract11 && conversationNumericId !== null ? (
                      <Pressable accessibilityRole="button" accessibilityLabel="Participants" onPress={() => setPeopleOpen(true)} hitSlop={10}>
                        <Text maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.action, { color: palette.accent }]}>
                          People
                        </Text>
                      </Pressable>
                    ) : null}
                  </View>
                )
              : undefined,
        }}
      />
      {projectId !== null && conversationNumericId !== null ? (
        <ParticipantsSheet
          visible={peopleOpen}
          onClose={() => setPeopleOpen(false)}
          projectId={projectId}
          conversationId={conversationNumericId}
          participants={detail?.participants ?? []}
          selfUserId={me}
          offline={!online}
          canAddAgent={agentsCanJoin(detail)}
          onChanged={() => void queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, ref) })}
          onAgentAdded={(participantId) => chosenKey !== null && setTarget(chosenKey, participantId)}
        />
      ) : null}
      <OfflineNotice visible={isOfflineCopy(detail)} />
      <FlatList
        inverted
        data={rows}
        keyExtractor={(row) => row.key}
        contentContainerStyle={styles.list}
        keyboardShouldPersistTaps="handled"
        renderItem={({ item }) => {
          switch (item.kind) {
            case 'persisted':
              return (
                <PersistedMessage
                  message={item.message}
                  highlight={highlight}
                  opening={opener.opening}
                  onOpenFile={canOpenFiles ? (a) => void opener.open(a) : null}
                  tools={
                    item.message.groupId !== null
                      ? markPausedSteps(traces.data?.get(item.message.groupId) ?? [], item.message.key === newest?.key ? persistedPause : null)
                      : []
                  }
                  onOpenTool={setOpenStep}
                  onActions={() => setActionsFor(item.message)}
                />
              );
            case 'live_user':
              return (
                <MessageBubble
                  role="user"
                  highlight={highlight}
                  status={item.files.length > 0 ? `Attached: ${item.files.join(', ')}` : null}>
                  {item.text}
                </MessageBubble>
              );
            case 'outbox':
              return <QueuedMessage item={item.item} onSendAgain={turn.send} />;
            case 'live_reply':
              return (
                <LiveReply
                  live={item.live}
                  author={target.ok ? target.name : null}
                  onRetry={turn.retry}
                  onReconnect={turn.reconnect}
                  onDismiss={turn.dismiss}
                  onOpenTool={setOpenStep}
                  usageAvailable={contract13}
                />
              );
          }
        }}
        ListEmptyComponent={
          <Text style={[styles.empty, { color: palette.textSecondary }]}>No messages yet.</Text>
        }
      />
      {pause && !(live && (live.turn.phase === 'sending' || live.turn.phase === 'streaming')) ? (
        <PauseCard
          key={`${pause.kind}:${pause.messageId ?? ''}`}
          pause={pause}
          target={pauseTarget}
          offline={!online}
          onContinue={turn.continueWith}
          onOpenWeb={webUrl ? () => void Linking.openURL(webUrl) : null}
        />
      ) : null}
      <ImageViewer uri={opener.viewer?.uri ?? null} name={opener.viewer?.name ?? ''} onClose={opener.closeViewer} />
      <ToolDetailSheet step={openStep} projectId={projectId} onClose={() => setOpenStep(null)} />
      <MessageActionsSheet
        message={actionsFor}
        plan={actionPlan}
        projectId={projectId}
        onClose={() => setActionsFor(null)}
        onRegenerate={(message) => void regenerate(message)}
        onReadAloud={(message) => readAloud(message.key, message.text)}
      />
      {reading !== null ? (
        <View style={[styles.reading, { borderTopColor: palette.border, backgroundColor: palette.backgroundElement }]}>
          <Text accessibilityLiveRegion="polite" style={[styles.readingText, { color: palette.textSecondary }]}>
            Reading aloud…
          </Text>
          <Pressable accessibilityRole="button" accessibilityLabel="Stop reading aloud" onPress={stopReading} hitSlop={10}>
            <Text style={[styles.actionSmall, { color: palette.accent }]}>Stop</Text>
          </Pressable>
        </View>
      ) : null}
      {threadNote ? (
        <Pressable accessibilityRole="button" accessibilityHint="Dismisses the note" onPress={() => setThreadNote(null)}>
          <Text accessibilityRole="alert" style={[styles.note, { color: palette.danger }]}>
            {threadNote}
          </Text>
        </Pressable>
      ) : null}
      <Composer
        disabledReason={disabledReason}
        busy={turn.busy}
        people={people}
        hint={mentionHint}
        externalTriggers={externalTriggers}
        onTriggerChange={setTrigger}
        externalSuggestions={suggestionState}
        onPickExternal={pickExternal}
        accessory={
          <>
            {target.ok && target.kind === 'agent' && target.choices.length > 1 && chosenKey !== null ? (
              <TargetChip current={target.agent} choices={target.choices} onChoose={(id) => setTarget(chosenKey, id)} />
            ) : null}
            <AttachmentTrayView items={trayItems} onRemove={(id) => tray.remove(id)} onRetry={(id) => tray.retry(id)} />
          </>
        }
        allowEmptyText={trayItems.length > 0}
        leading={<AttachButton disabledReason={gateReason} onPick={(source) => void pickInto(source)} />}
        onSend={({ text, mentions }) => sendWithFiles(text, mentions)}
        onStop={contract13 && online && turn.canStop ? turn.stop : null}
        dictation={dictation}
      />
    </KeyboardAvoidingView>
  );
}

function PersistedMessage({
  message,
  tools,
  onOpenTool,
  highlight,
  opening,
  onOpenFile,
  onActions,
}: {
  message: ChatMessage;
  onActions: () => void;
  highlight: { people: string[]; agents: string[] };
  opening: string | null;
  onOpenFile: ((attachment: ChatAttachment) => void) | null;
  tools: readonly ToolStep[];
  onOpenTool: (step: ToolStep) => void;
}) {
  const status = message.isStreaming
    ? 'Still generating…'
    : message.hasUnsupportedItems
      ? 'Part of this message can only be viewed in the web app.'
      : null;
  return (
    <MessageBubble
      role={message.role}
      mine={message.mine}
      author={message.author}
      status={status}
      error={message.error}
      highlight={message.role === 'user' ? highlight : null}
      onActions={message.text.trim() !== '' || message.role === 'assistant' ? onActions : null}
      footer={
        message.attachments.length > 0 || tools.length > 0 ? (
          <>
            <AttachmentStrip attachments={message.attachments} mine={message.mine} opening={opening} onOpen={onOpenFile} />
            {tools.length > 0 ? <ToolActivity steps={tools} onOpen={onOpenTool} /> : null}
          </>
        ) : null
      }>
      {message.role === 'assistant' && message.text !== '' ? <Markdown value={message.text} /> : message.text}
    </MessageBubble>
  );
}

/** A message waiting in the offline outbox (queued, sending, or refused). */
/** What a refused outbox item carried besides its text (kept on "Send again"). */
function extrasOf(item: OutboxItem): SendExtras {
  try {
    const { mentions, attachments } = restorePreparedTurn(item.bodyJson).request;
    return { mentions, attachments };
  } catch (error) {
    if (error instanceof TurnInputError) return {};
    throw error;
  }
}

function QueuedMessage({
  item,
  onSendAgain,
}: {
  item: OutboxItem;
  onSendAgain: (text: string, extras?: SendExtras) => Promise<string | null>;
}) {
  const palette = usePalette();
  const status = outboxStatus(item);
  const actions =
    item.state === 'failed'
      ? [
          // A new message with a new question_id; the refused one is dropped
          // only once the new one is accepted, so a failed resend loses nothing.
          {
            label: 'Send again',
            onPress: () =>
              void onSendAgain(item.userText, extrasOf(item)).then((problem) => {
                if (problem === null) return outboxDrainer.remove(item.questionId);
              }),
          },
          { label: 'Delete', onPress: () => void outboxDrainer.remove(item.questionId) },
        ]
      : item.state === 'queued'
        ? [
            { label: 'Retry now', onPress: () => void outboxDrainer.retryNow(item.questionId) },
            { label: 'Delete', onPress: () => void outboxDrainer.remove(item.questionId) },
          ]
        : [];
  return (
    <MessageBubble
      role="user"
      status={status.error ? null : status.text}
      error={status.error ? status.text : null}
      footer={
        actions.length > 0 ? (
          <View style={styles.actions}>
            {actions.map((a) => (
              <Pressable key={a.label} accessibilityRole="button" onPress={a.onPress}>
                <Text style={[styles.actionSmall, { color: palette.accent }]}>{a.label}</Text>
              </Pressable>
            ))}
          </View>
        ) : null
      }>
      {item.userText}
    </MessageBubble>
  );
}

function LiveReply({
  live,
  author,
  onRetry,
  onReconnect,
  onDismiss,
  onOpenTool,
  usageAvailable,
}: {
  live: LiveTurn;
  author: string | null;
  onRetry: () => void;
  onReconnect: () => void;
  onDismiss: () => void;
  onOpenTool: (step: ToolStep) => void;
  /** Settings has a usage card (client contract 1.3). Older servers refuse on budget too, but have no usage to show. */
  usageAvailable: boolean;
}) {
  const palette = usePalette();
  const { turn } = live;
  const waiting = turn.phase === 'sending' || (turn.phase === 'streaming' && turn.content === '');
  const status =
    turn.status ??
    (turn.stopped
      ? 'Stopped.'
      : turn.phase === 'paused'
      ? turn.pause
        ? 'Waiting for you.'
        : 'Paused. Continue it in the web app.'
      : turn.phase === 'sending'
        ? 'Sending…'
        : waiting
          ? 'Thinking…'
          : live.streamStatus === 'retrying'
            ? 'Reconnecting…'
            : null);
  const actions = [
    live.canReconnect ? { label: 'Reconnect', onPress: onReconnect } : null,
    turn.phase === 'failed' && turn.error?.retryable ? { label: 'Retry', onPress: onRetry } : null,
    turn.phase === 'failed' && turn.error?.budget && usageAvailable ? { label: 'View usage', onPress: () => router.push('/settings') } : null,
    turn.phase === 'failed' ? { label: 'Dismiss', onPress: onDismiss } : null,
  ].filter((a): a is { label: string; onPress: () => void } => a !== null);
  return (
    <MessageBubble
      role="assistant"
      author={author}
      status={status}
      error={turn.phase === 'failed' ? (turn.error?.message ?? null) : null}
      footer={
        <>
          <ToolActivity steps={turn.tools} streaming={turn.phase === 'streaming'} onOpen={onOpenTool} />
          {actions.length > 0 ? (
            <View style={styles.actions}>
              {actions.map((a) => (
                <Pressable key={a.label} accessibilityRole="button" onPress={a.onPress}>
                  <Text style={[styles.actionSmall, { color: palette.accent }]}>{a.label}</Text>
                </Pressable>
              ))}
            </View>
          ) : null}
        </>
      }>
      {turn.content !== '' ? <Markdown value={turn.content} streaming={turn.phase === 'streaming'} /> : ''}
    </MessageBubble>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  list: { paddingVertical: Spacing.two },
  // The list is inverted, so flip the empty state back upright.
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15, transform: [{ scaleY: -1 }] },
  action: { fontSize: 16, fontWeight: '600' },
  actions: { flexDirection: 'row', gap: Spacing.three, marginTop: Spacing.one },
  actionSmall: { fontSize: 14, fontWeight: '600' },
  note: { fontSize: 14, paddingHorizontal: Spacing.three, paddingVertical: Spacing.one },
  reading: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.one,
    borderTopWidth: StyleSheet.hairlineWidth,
  },
  readingText: { fontSize: 14 },
  headerActions: { flexDirection: 'row', gap: Spacing.three, alignItems: 'center' },
});
