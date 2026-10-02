/**
 * The pipeline editor's live test chat — `ConfigurationTab`'s `renderChat`
 * slot, filled with the REAL `widgets/chat-box` `ChatBox`.
 *
 * **THE CLAIM THIS REPLACES WAS STALE, NOT MERELY OPTIMISTIC.**
 * `lib/pipelineConfigurationTabGaps.tsx` used to say the slot "needs
 * `features/chat`'s `ChatBox` — that slice does not exist". Two things were
 * wrong with that. The chat composition root is `widgets/chat-box`, not
 * `features/chat`; and the slot is rendered by the PAGE, which may import
 * `widgets/` freely (the `no-upward-from-features` rule that forbids it
 * applies to `features/pipelines`, which is why the slot exists at all —
 * `ChatPanel.tsx`'s own doc comment). So the one layer that is allowed to
 * fill this slot is exactly the layer that owns it.
 *
 * **THE IMPERATIVE CONTRACT LINES UP ALREADY.** `ChatPanel` declares what
 * the slot owes as `ChatBoxSlotHandle` (`stopAll`/`onClear`); `ChatBoxHandle`
 * is `stopAll`/`onClear`/`mentionUser` — a structural superset. The two are
 * bridged through an explicit `useImperativeHandle` rather than by handing
 * the slot's ref straight to `ChatBox`: a `RefObject` is invariant, so the
 * narrower ref is not assignable to the wider one even though every value
 * flowing through it satisfies both.
 *
 * **THE RUN FEED.** `settings.onRcvAgentEvent` is the ALREADY-BUILT receive
 * side — `ConfigurationTab` -> `EditorPanel` -> `FlowEditor` -> `useRunEvent`
 * -> the node highlight (`data.isPerforming`) and the "Run N details" node.
 * Nothing fed it. `ChatBox`'s `extensions.onAgentEvent` (added with this
 * change) is the wire: `useChatStreamTransport` forwards exactly the frames
 * `shouldForwardAgentEvent` admits, so `agent_on_*`/tool/LLM lifecycle frames
 * reach the canvas and per-token `agent_llm_chunk` frames do not.
 */
import type { ReactNode } from 'react';
import { useCallback, useImperativeHandle, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConfigurationTabProps } from '@/features/pipelines';
import type { ExecutionEventData } from '@/shared/api/sse';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { ChatBox, type ChatBoxHandle } from '@/widgets/chat-box';

import { usePipelineTestConversation, type PipelineTestChatIdentity } from '../lib/usePipelineTestConversation';

/** `ChatPanel`'s own slot-props type, reached through the barrel-exported `ConfigurationTabProps` (`ChatPanelSlotProps`/`ChatBoxSlotHandle` are not on `features/pipelines`' public API). */
type ChatSlotProps = Parameters<ConfigurationTabProps['slots']['renderChat']>[0];

export interface PipelineTestChatProps {
  /** `usePipelineChat`'s result plus the version's own fields — see `ConfigurationTab`'s `useConfigurationTabSettings`. */
  readonly settings: ChatSlotProps['settings'];
  /** `ChatPanel`'s own gate: no application id yet, or the flow editor holds unsaved YAML. */
  readonly disableChat: boolean;
  readonly slotRef: ChatSlotProps['ref'];
  readonly identity: Omit<PipelineTestChatIdentity, 'userId'>;
  /** The signed-in user, resolved by the page (the author read is the same one the Chat button already performs). */
  readonly user: { readonly id?: string; readonly name?: string; readonly avatar?: string } | undefined;
}

const paneSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0, width: '100%' };
const noticeSx: SxProps<Theme> = { padding: '1.5rem', height: '100%', boxSizing: 'border-box' };

/** `settings.onRcvAgentEvent` — `ConfigurationTab` puts it on the settings bag, which is typed as a loose record. */
function readAgentEventSink(settings: ChatSlotProps['settings']): ((event: unknown) => void) | undefined {
  const sink = settings['onRcvAgentEvent'];
  return typeof sink === 'function' ? (sink as (event: unknown) => void) : undefined;
}

/**
 * `settings.conversationStarters`, narrowed to the strings `ChatBox` renders.
 *
 * This used to keep only `{id, text}` rows, which discarded EVERY real
 * starter: the version detail stores them as plain strings, and
 * `ChatConversationStarters` stringifies whatever it is given anyway. The
 * `{id, text}` shape came from `ChatBox`'s own declared prop type, which was
 * wrong at both ends and is now `readonly string[]`.
 */
function readConversationStarters(settings: ChatSlotProps['settings']): readonly string[] {
  const starters = settings['conversationStarters'];
  if (!Array.isArray(starters)) return [];
  return starters.filter((entry): entry is string => typeof entry === 'string');
}

/** Route model changes through the editor so unsaved settings cannot silently change a test run. */
function readLlmSettings(settings: ChatSlotProps['settings']) {
  const values = settings['llmSettings'];
  const update = settings['onSetLLMSettings'];
  return {
    ...(values && typeof values === 'object' && !Array.isArray(values)
      ? { settings: values as Readonly<Record<string, unknown>> } : {}),
    ...(typeof update === 'function'
      ? { onSetSettings: update as (values: Readonly<Record<string, unknown>>) => void } : {}),
  };
}

export function PipelineTestChat({ settings, disableChat, slotRef, identity, user }: PipelineTestChatProps): ReactNode {
  const userId = user?.id;
  const chatIdentity = useMemo<PipelineTestChatIdentity>(
    () => ({ ...identity, ...(userId !== undefined ? { userId } : { userId: undefined }) }),
    [identity, userId],
  );
  const testConversation = usePipelineTestConversation(chatIdentity);

  const chatBoxRef = useRef<ChatBoxHandle | null>(null);
  useImperativeHandle(
    slotRef,
    () => ({
      stopAll: () => chatBoxRef.current?.stopAll(),
      onClear: () => chatBoxRef.current?.onClear(),
    }),
    [],
  );

  const agentEventSink = readAgentEventSink(settings);
  const handleAgentEvent = useCallback(
    (frame: ExecutionEventData) => {
      agentEventSink?.(frame);
    },
    [agentEventSink],
  );

  const ensure = testConversation.ensure;
  const conversationStarters = readConversationStarters(settings);

  if (disableChat) {
    return (
      <Box
        data-testid="edit-pipeline-test-chat-disabled"
        sx={noticeSx}
      >
        <NoResultsMessage
          title={t('pages.pipelines.testChat.disabled.title', 'Save the pipeline to test it')}
          description={t(
            'pages.pipelines.testChat.disabled.description',
            'This chat runs the version stored on the server, so it stays closed while the editor holds unsaved changes.',
          )}
        />
      </Box>
    );
  }

  return (
    <Box
      data-testid="edit-pipeline-test-chat"
      sx={paneSx}
      // The conversation is created on the FIRST interaction with this pane,
      // not on mount — opening the editor to read a graph must not mint a
      // conversation row. Capture-phase, so it fires before the composer's
      // own handlers and the bootstrap is already in flight by the time the
      // first keystroke lands. `usePipelineTestConversation.ensure` is
      // idempotent, so both handlers firing for one click costs nothing.
      onPointerDownCapture={ensure}
      onFocusCapture={ensure}
      // Also on the first keystroke, which is the retry that matters: if the
      // pane was clicked BEFORE the version or the author read resolved,
      // `ensure` no-opped and left itself armed — without this the user would
      // have to click away and back to get a conversation.
      onKeyDownCapture={ensure}
    >
      {testConversation.staleVersionId !== undefined && (
        // The version is SERVER-side state on the participant row, so a failed
        // switch means turns keep running the old graph while the canvas shows
        // the new one. Silence here is the one outcome nobody can debug.
        <Box
          role="alert"
          data-testid="edit-pipeline-test-chat-stale-version"
          sx={{ px: 2, pt: 1 }}
        >
          {t(
            'pages.pipelines.testChat.staleVersion',
            'Could not switch this chat to the selected version. Replies still run version {{versionId}}.',
            { versionId: testConversation.staleVersionId },
          )}
        </Box>
      )}
      {testConversation.hasFailed && (
        <Box
          role="alert"
          data-testid="edit-pipeline-test-chat-error"
          sx={{ px: 2, pt: 1 }}
        >
          {t('pages.pipelines.testChat.error', 'Could not start a test conversation. Click the message box to try again.')}
        </Box>
      )}
      <ChatBox
        ref={chatBoxRef}
        conversation={{
          ...(testConversation.conversation ? { active: testConversation.conversation } : {}),
          isLoading: testConversation.isCreating,
        }}
        {...(identity.projectId !== undefined ? { projectId: identity.projectId } : {})}
        {...(user ? { user } : {})}
        participant={{ active: testConversation.activeParticipant }}
        conversationStarters={conversationStarters}
        llm={readLlmSettings(settings)}
        // The editor's pane IS the agent/pipeline test surface the baseline
        // means by this flag: it suppresses the ad-hoc `dummy` model
        // participant, because the turn is addressed to the pipeline.
        isAgentsPage
        extensions={{ onAgentEvent: handleAgentEvent }}
      />
    </Box>
  );
}
