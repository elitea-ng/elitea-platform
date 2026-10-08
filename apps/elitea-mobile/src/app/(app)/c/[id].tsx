import { Stack, useLocalSearchParams } from 'expo-router';
import * as Linking from 'expo-linking';
import { useCallback, useEffect, useMemo, useRef, useState, type ComponentRef } from 'react';
import { AccessibilityInfo, ActivityIndicator, Alert, FlatList, Pressable, StyleSheet, View } from 'react-native';

import { isConversationRef, useConversation } from '@/api/queries/conversation';
import { useModels } from '@/api/queries/models';
import type { TurnModel } from '@/api/queries/sendMessage';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { usePalette } from '@/constants/use-palette';
import { useDesign } from '@/design/DesignProvider';
import { GlassSurface, headerActionOptions, Icon, IconButton, Text } from '@/ui';
import { Composer } from '@/features/chat/Composer';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { speaksContract } from '@/api/discovery';
import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { AssistantAvatar, useAgentIconUri } from '@/features/chat/AssistantAvatar';
import { ChatEmptyState, MODEL_CHAT_SUGGESTIONS } from '@/features/chat/ChatEmptyState';
import { ThreadLayout, Toast, useToast } from '@/features/chat/ThreadLayout';
import { LiveReply, PersistedMessage, QueuedMessage } from '@/features/chat/ThreadRows';
import { displayHost } from '@/features/connect/connectFlow';
import type { ActiveTrigger, MentionCandidate, TriggerChar } from '@/features/chat/mentions/mentions';
import {
  chatTarget,
  agentOptions,
  agentsCanJoin,
  participantName,
  participantUserId,
  toChatMessages,
  type ChatMessage,
} from '@/features/chat/messageModel';
import type { SendExtras, StopResult } from '@/features/chat/useTurn';
import { useMessageTraces } from '@/api/queries/messageTraces';
import { ToolDetailSheet } from '@/features/chat/ToolDetailSheet';
import { markPausedSteps, type ToolStep, type TurnPause } from '@/features/chat/toolActivity';
import { PauseCard } from '@/features/chat/pauses/PauseCard';
import { useQueryClient } from '@tanstack/react-query';
import { useAgents, useAgentVersion } from '@/api/queries/agents';
import { AttachButton, AttachmentTrayView, type PickSource } from '@/features/chat/attachments/AttachmentTrayView';
import { attachmentGate, handoffPick, type GateInput } from '@/features/chat/attachments/gate';
import { PickerPermissionError, pickFiles, pickPhotos, takePhoto } from '@/features/chat/attachments/pickers';
import { attachmentLimits, type PickedFile } from '@/features/chat/attachments/prepare';
import { useDocumentScan } from '@/features/chat/attachments/scan/useDocumentScan';
import { useAttachmentTray } from '@/features/chat/attachments/useAttachmentTray';
import { ImageViewer } from '@/features/chat/attachments/AttachmentStrip';
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
import { markThenDone, readHandoff } from '@/features/intents/intentHandoff';
import { MAX_PREFILL } from '@/features/intents/intentRouter';
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
import { answerHiddenByRegeneration, newQuestionId, type LiveTurn } from '@/features/chat/turnController';
import { ATTACHMENT_ONLY_TEXT, useTurn } from '@/features/chat/useTurn';
import { useSelfUserId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';
import { useOutboxItems } from '@/sync/appOutbox';
import { isOfflineCopy } from '@/sync/offlineReads';
import type { OutboxItem } from '@/sync/outbox';

type Row =
  | { kind: 'persisted'; key: string; message: ChatMessage }
  | { kind: 'outbox'; key: string; item: OutboxItem }
  | { kind: 'live_user'; key: string; text: string; files: string[] }
  | { kind: 'live_reply'; key: string; live: LiveTurn };

/** One conversation: its latest messages, newest at the bottom, and the composer. */
export default function ThreadScreen() {
  const palette = usePalette();
  const design = useDesign();
  // The question an intent handed over in memory (intentHandoff.ts); never text from a URL.
  const { id, intent } = useLocalSearchParams<{ id: string; intent?: string }>();
  const prefill = readHandoff(intent)?.prefill ?? null;
  // Home's "Dictate a message" / "Ask about a photo/file": what to do once, now that it is open.
  const [then] = useState(() => {
    const action = readHandoff(intent)?.then ?? null;
    if (action !== null) markThenDone(intent);
    return action;
  });
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
  const [actionsFor, setActionsFor] = useState<{ message: ChatMessage; dislike: boolean } | null>(null);
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
  /**
   * The note above the composer: an error (a refusal, a failure: danger, read as an alert), or
   * information (a file attached with a caveat: secondary, no alert).
   */
  const [threadNote, setNote] = useState<{ text: string; kind: 'error' | 'info' } | null>(null);
  const setThreadNote = useCallback((text: string | null) => setNote(text === null ? null : { text, kind: 'error' }), []);
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
  const actionPlan = planActions(actionsFor?.message ?? { role: 'user', text: '', uuid: null, isStreaming: false }, {
    shareOut: policy.allow_share_out,
    contract13,
    online: online && !isOfflineCopy(detail),
    canSpeak: canReadAloud(),
    canRegenerate: canRegenerate(actionsFor?.message ?? null),
  });

  // Attachments (contract 1.1 limits, else the server defaults): picked and
  // prepared now, uploaded on send. Online only; an agent must take files.
  const limits = attachmentLimits(workspace?.discovery.attachments);
  const { tray, items: trayItems } = useAttachmentTray(projectId, conversationUuid, limits);
  const targetAgent = target.ok && target.kind === 'agent' ? target.agent : null;
  const agentVersion = useAgentVersion(targetAgent ? (targetAgent.agentProjectId ?? projectId) : null, targetAgent?.agentId ?? null);
  const gateInput: GateInput = {
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
  };
  const gateReason = attachmentGate(gateInput);
  // Files of sent messages: downloaded on demand (1.1 route), never kept.
  const opener = useAttachmentOpener(projectId, conversationUuid);
  const canOpenFiles = contract11 && online && !isOfflineCopy(detail);

  // "Scan document" (iOS): VisionKit, or the sample pages in a development build on the simulator.
  const documentScan = useDocumentScan(limits);
  const scanInto = async (): Promise<{ files: PickedFile[]; warnings: string[] } | null> => {
    const outcome = await documentScan.scan(() => tray.usage());
    if (outcome.kind === 'refused') {
      setThreadNote(outcome.message);
      return null;
    }
    return outcome.kind === 'files' ? outcome : null;
  };
  // While a scan is up or being made, another attach waits, and a send is held (it would go
  // without the scan).
  const scanBusy = documentScan.phase !== 'idle';
  const attachReason = gateReason ?? (scanBusy ? 'The scan is still being prepared.' : null);
  const pickInto = async (source: PickSource) => {
    try {
      let scanWarnings: string[] = [];
      let picked: PickedFile[];
      if (source === 'scan') {
        const scanned = await scanInto();
        if (scanned === null) return;
        picked = scanned.files;
        scanWarnings = scanned.warnings;
      } else {
        picked =
          source === 'photos' ? await pickPhotos(limits.maxFiles - tray.sendable) : source === 'camera' ? await takePhoto() : await pickFiles();
      }
      if (picked.length === 0) return;
      const added = tray.add(picked);
      const refused = added.refused;
      const warnings = [...added.warnings, ...(refused.length === 0 ? scanWarnings : [])];
      // Success is silent: the file is in the tray (audit X8). A refusal is an error note above
      // the composer, not a modal alert; a warning alone (the file IS attached, with a caveat)
      // is an information note, not an error.
      if (refused.length > 0) setThreadNote([...refused.map((r) => r.message), ...warnings].join('\n'));
      else if (warnings.length > 0) {
        const text = warnings.join('\n');
        setNote({ text, kind: 'info' });
        AccessibilityInfo.announceForAccessibility(text);
      }
    } catch (error) {
      if (error instanceof PickerPermissionError) {
        setThreadNote('Allow camera access in Settings to take a photo here.');
        return;
      }
      setThreadNote(source === 'scan' ? "The scan couldn't be saved. Try again." : "Those files couldn't be opened.");
    }
  };
  const pickedOnOpen = useRef(false);
  useEffect(() => {
    if (pickedOnOpen.current) return;
    const plan = handoffPick(then, gateInput);
    if (plan.kind === 'none' || plan.kind === 'wait') return;
    pickedOnOpen.current = true;
    // The picker opens after this render (its notes are state, never set from inside an effect).
    if (plan.kind === 'pick') queueMicrotask(() => void pickInto(plan.source));
    else Alert.alert(plan.title, plan.message);
  });
  const sendWithFiles = async (text: string, mentions: SendExtras['mentions']): Promise<string | null> => {
    if (scanBusy) return 'The scan is still being prepared. Send once it is in the tray.';
    if (trayItems.length === 0) return turn.send(text, { mentions });
    if (gateReason) return gateReason;
    // Every file first: a message with files is sent (or queued) only once all are stored.
    const uploaded = await tray.uploadAll();
    if (!uploaded.ok) return uploaded.error;
    // Minted here so the held files can be tied to this question.
    const questionId = newQuestionId();
    const problem = await turn.send(text.trim() === '' ? ATTACHMENT_ONLY_TEXT : text, {
      mentions,
      attachments: uploaded.attachments,
      questionId,
    });
    if (problem !== null) return problem;
    // Held, not deleted, while its answer runs: a Stop before any output
    // removes the question, and its files then come back (stopWithFiles).
    // A send queued for the outbox (or already handed to it) cannot be
    // stopped here: the outbox keeps the uploaded filepaths, not these copies.
    if (turn.isRunning(questionId)) tray.holdSent(uploaded.ids, questionId);
    else tray.clear(uploaded.ids);
    return null;
  };
  // Let go of the held files once their question can no longer be stopped
  // here: its answer ended (an answer, a failure the retry re-sends with the
  // stored files, a pause, or a stop made elsewhere), or the live turn is no
  // longer that question (handed off to the outbox, dismissed, another turn).
  const livePhase = turn.live?.turn.phase ?? null;
  const liveQuestion = turn.live?.questionId ?? null;
  // A Stop of ours is running: stopWithFiles decides (the turn ends stopped,
  // and may be dismissed, before it knows whether the question was removed).
  const [stopping, setStopping] = useState(false);
  useEffect(() => {
    const heldFor = tray.heldFor;
    if (heldFor === null || stopping) return;
    const ended = livePhase === 'failed' || livePhase === 'paused' || livePhase === 'done';
    if (ended || liveQuestion !== heldFor) tray.releaseSent();
  }, [tray, livePhase, liveQuestion, stopping]);
  const stopWithFiles = async (): Promise<StopResult> => {
    setStopping(true);
    try {
      return await stopAndRestore();
    } finally {
      setStopping(false);
    }
  };
  const stopAndRestore = async (): Promise<StopResult> => {
    const result = await turn.stop();
    if (result.restoreText !== null && result.lostAttachments.length > 0) {
      // Only what the tray actually put back is off the list: a file it no
      // longer held (the thread was left and reopened while the answer ran,
      // or a retried send) is still named, never dropped silently.
      const { restored } = tray.restoreSent();
      const lost = [...result.lostAttachments];
      for (const name of restored) {
        const at = lost.indexOf(name);
        if (at !== -1) lost.splice(at, 1);
      }
      return { ...result, lostAttachments: lost, restoredAttachments: restored };
    }
    if (result.problem === null) tray.releaseSent();
    return result;
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

  // Who answers, drawn left of every answer (§4.6, §5): the addressed agent's own icon; else, for
  // it (or a model chat), Agent Zefirca; another agent of the conversation gets a monogram.
  const agentIcon = useAgentIconUri(targetAgent ? (targetAgent.agentProjectId ?? projectId) : null, agentVersion.data?.iconPath ?? null);
  const primaryParticipant = target.ok ? target.participantId : null;
  const avatarFor = (participantId: number | null, name: string | null) => {
    const primary = participantId === null || participantId === primaryParticipant;
    const label = name ?? (target.ok ? target.name : null) ?? 'Assistant';
    return <AssistantAvatar name={label} imageUri={primary ? agentIcon : null} mascot={primary} />;
  };
  const { toast, show: showToast, hide: hideToast } = useToast();
  const [draft, setDraft] = useState<{ text: string; key: number } | null>(null);
  const list = useRef<ComponentRef<typeof FlatList<Row>>>(null);
  const [scrolledAway, setScrolledAway] = useState(false);

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
          <Text variant="headline" tone="accent">
            Retry
          </Text>
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

  const targetName = target.ok ? target.name : null;
  const pauseCard =
    pause && !(live && (live.turn.phase === 'sending' || live.turn.phase === 'streaming')) ? (
      <PauseCard
        key={`${pause.kind}:${pause.messageId ?? ''}`}
        pause={pause}
        target={pauseTarget}
        offline={!online}
        host={workspace ? displayHost(workspace.origin) : null}
        onContinue={turn.continueWith}
        onOpenWeb={webUrl ? () => void Linking.openURL(webUrl) : null}
      />
    ) : null;
  const empty = rows.length === 0 && pauseCard === null;
  const suggestions = targetAgent ? (agentVersion.data?.starters ?? []) : target.ok ? MODEL_CHAT_SUGGESTIONS : [];

  return (
    <>
      <Stack.Screen
        options={{
          title: detail?.name || 'Conversation',
          ...headerActionOptions(
            [
              ...(canExport
                ? [{ id: 'export', icon: 'share' as const, label: 'Export conversation', disabled: exporting, busy: exporting, onPress: () => void exportTranscript() }]
                : []),
              ...(contract11 && conversationNumericId !== null
                ? [{ id: 'people', icon: 'people' as const, label: 'Participants', onPress: () => setPeopleOpen(true) }]
                : []),
            ],
            design,
          ),
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
      <ThreadLayout
        jumpToLatest={scrolledAway && !empty ? () => list.current?.scrollToOffset({ offset: 0, animated: !design.reduceMotion }) : null}
        renderTranscript={(composerHeight) => (
          <>
            <OfflineNotice visible={isOfflineCopy(detail)} />
            {empty ? (
              <ChatEmptyState
                agentName={targetName ?? 'your agent'}
                iconUri={agentIcon}
                welcome={agentVersion.data?.welcomeMessage ?? null}
                suggestions={disabledReason ? [] : suggestions}
                onSuggestion={(text) => setDraft((current) => ({ text, key: (current?.key ?? 0) + 1 }))}
                bottomInset={composerHeight}
              />
            ) : (
              <FlatList
                ref={list}
                inverted
                data={rows}
                keyExtractor={(row) => row.key}
                // Inverted: the "top" padding is the bottom edge, under the floating composer.
                contentContainerStyle={[styles.list, { paddingTop: composerHeight + design.space[2] }]}
                keyboardShouldPersistTaps="handled"
                keyboardDismissMode="interactive"
                onScroll={(event) => setScrolledAway(event.nativeEvent.contentOffset.y > 480)}
                scrollEventThrottle={100}
                // The pause card closes the transcript, inline after the newest answer (§4.7).
                ListHeaderComponent={pauseCard ?? undefined}
                renderItem={({ item }) => {
                  switch (item.kind) {
                    case 'persisted': {
                      const message = item.message;
                      const plan = planActions(message, {
                        shareOut: policy.allow_share_out,
                        contract13,
                        online: online && !isOfflineCopy(detail),
                        canSpeak: canReadAloud(),
                        canRegenerate: canRegenerate(message),
                      });
                      return (
                        <PersistedMessage
                          message={message}
                          highlight={highlight}
                          opening={opener.opening}
                          onOpenFile={canOpenFiles ? (a) => void opener.open(a) : null}
                          tools={
                            message.groupId !== null
                              ? markPausedSteps(traces.data?.get(message.groupId) ?? [], message.key === newest?.key ? persistedPause : null)
                              : []
                          }
                          onOpenTool={setOpenStep}
                          onActions={() => setActionsFor({ message, dislike: false })}
                          avatar={message.role === 'assistant' ? avatarFor(message.authorParticipantId, message.author) : undefined}
                          actions={
                            message.role === 'assistant' ? (
                              <AnswerActions
                                message={message}
                                plan={plan}
                                projectId={projectId}
                                reading={reading === message.key}
                                onReadAloud={() => readAloud(message.key, message.text)}
                                onStopReading={stopReading}
                                onRegenerate={() => void regenerate(message)}
                                onMore={() => setActionsFor({ message, dislike: false })}
                                onDislike={() => setActionsFor({ message, dislike: true })}
                                onNote={showToast}
                                loadRating={message.key === newestAnswer?.key}
                              />
                            ) : undefined
                          }
                        />
                      );
                    }
                    case 'live_user':
                      return (
                        <MessageBubble role="user" highlight={highlight} status={item.files.length > 0 ? `Attached: ${item.files.join(', ')}` : null}>
                          {item.text}
                        </MessageBubble>
                      );
                    case 'outbox':
                      return <QueuedMessage item={item.item} onSendAgain={turn.send} />;
                    case 'live_reply':
                      return (
                        <LiveReply
                          live={item.live}
                          author={targetName}
                          avatar={avatarFor(primaryParticipant, targetName)}
                          onRetry={turn.retry}
                          onReconnect={turn.reconnect}
                          onDismiss={turn.dismiss}
                          onOpenTool={setOpenStep}
                          usageAvailable={contract13}
                        />
                      );
                  }
                }}
              />
            )}
          </>
        )}
        floating={
          <>
            <Toast toast={toast} onHide={hideToast} />
            {reading !== null ? (
              <GlassSurface radius={design.radius.pill} floating style={styles.reading}>
                <Icon name="readAloud" size={18} tone="accent" />
                <Text variant="subheadline" accessibilityLiveRegion="polite">
                  Reading aloud…
                </Text>
                <IconButton icon="stop" variant="tonal" size={32} iconSize={14} accessibilityLabel="Stop reading aloud" onPress={stopReading} />
              </GlassSurface>
            ) : null}
            {threadNote ? (
              <Pressable accessibilityRole="button" accessibilityHint="Dismisses the note" onPress={() => setThreadNote(null)} style={styles.noteWrap}>
                <GlassSurface radius={design.radius.md} floating style={styles.note} testID={`thread-note-${threadNote.kind}`}>
                  <Icon name={threadNote.kind === 'error' ? 'error' : 'info'} size={18} tone={threadNote.kind === 'error' ? 'danger' : 'secondary'} />
                  <Text
                    variant="footnote"
                    tone={threadNote.kind === 'error' ? 'danger' : 'secondary'}
                    accessibilityRole={threadNote.kind === 'error' ? 'alert' : undefined}
                    style={styles.flex}>
                    {threadNote.text}
                  </Text>
                  <Icon name="close" size={14} tone="secondary" />
                </GlassSurface>
              </Pressable>
            ) : null}
          </>
        }
        composer={
          <Composer
            // Siri "Ask <agent>": the question waits in the input for the user to send it.
            initialText={typeof prefill === 'string' ? prefill.slice(0, MAX_PREFILL) : null}
            placeholder={targetName ? `Message ${targetName}…` : 'Message'}
            draft={draft}
            disabledReason={disabledReason}
            busy={turn.busy}
            people={people}
            hint={mentionHint}
            externalTriggers={externalTriggers}
            onTriggerChange={setTrigger}
            externalSuggestions={suggestionState}
            onPickExternal={pickExternal}
            accessory={
              (target.ok && target.kind === 'agent' && target.choices.length > 1 && chosenKey !== null) || trayItems.length > 0 || documentScan.phase === 'preparing' ? (
                <>
                  {target.ok && target.kind === 'agent' && target.choices.length > 1 && chosenKey !== null ? (
                    <TargetChip current={target.agent} choices={target.choices} onChoose={(id) => setTarget(chosenKey, id)} />
                  ) : null}
                  <AttachmentTrayView
                    items={trayItems}
                    pending={documentScan.phase === 'preparing' ? { name: 'Scan', meta: 'Preparing…' } : null}
                    onRemove={(id) => tray.remove(id)}
                    onRetry={(id) => tray.retry(id)}
                  />
                </>
              ) : null
            }
            allowEmptyText={trayItems.length > 0}
            leading={<AttachButton disabledReason={attachReason} scan={documentScan.source} onPick={(source) => void pickInto(source)} onBlocked={showToast} />}
            onSend={({ text, mentions }) => sendWithFiles(text, mentions)}
            onStop={contract13 && online && turn.canStop ? stopWithFiles : null}
            dictation={dictation}
            autoDictate={then === 'dictate' && dictation}
          />
        }
      />
      <ImageViewer uri={opener.viewer?.uri ?? null} name={opener.viewer?.name ?? ''} onClose={opener.closeViewer} />
      <ToolDetailSheet step={openStep} projectId={projectId} onClose={() => setOpenStep(null)} />
      <MessageActionsSheet
        message={actionsFor?.message ?? null}
        startWithDislike={actionsFor?.dislike ?? false}
        plan={actionPlan}
        projectId={projectId}
        onClose={() => setActionsFor(null)}
        onRegenerate={(message) => void regenerate(message)}
        onReadAloud={(message) => readAloud(message.key, message.text)}
      />
    </>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  list: { paddingBottom: 8 },
  reading: { flexDirection: 'row', alignItems: 'center', gap: 10, paddingStart: 14, paddingEnd: 6, paddingVertical: 6 },
  noteWrap: { alignSelf: 'stretch' },
  note: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, paddingVertical: 10 },
});
