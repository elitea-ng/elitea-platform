/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/
 * ApplicationAnswer.jsx` — the main AI answer message row component.
 *
 * `answer` here is the full `ChatMessage` (unlike the baseline, whose
 * `answer` prop was just the content string with `message_items`/
 * `exception`/`toolActions`/etc. as separate sibling props) — so fields the
 * baseline read off separate props are read directly off `answer` here
 * (`answer.exception`, `answer.messageItems`, `answer.content`).
 * `toolActions`/`hitlInterrupt(s)`/the continue-execution callbacks stay
 * explicit props (matching the baseline's own `ChatMessageWrapper` design)
 * since they are conversation-level data/callbacks the render loop supplies,
 * not intrinsic to a single message. Related props are grouped into option
 * objects (`status`/`actions`/`tts`/`continuation`/`hitl`) to stay under the
 * §3.5 component-props budget — see sibling `ActionView.tsx`/`entities/agents`
 * for the same established grouping pattern elsewhere in this Wave.
 *
 * Per-word TTS highlight sync (issue #625 item 1): `tts.spokenRange` reaches
 * `shared/ui/Markdown` now, but only for the row `tts.speakingMessageId`
 * names — `spokenRange` is one global offset range, not scoped to a message
 * id, so every other row must see `undefined` rather than highlight an
 * unrelated offset at the same position. `speakingSegments` still has no
 * reader; nothing in this pass needed it.
 *
 * Canvas message items ARE rendered now (issue 853): `./AnswerMessageItems`
 * walks `message_items` in their stored order and mounts the ported `Canvas`
 * block — the transcript's opener — for every `canvas_message`. Before that,
 * this component filtered the items down to `text_message` alone, so a canvas
 * carved out of an answer showed the two halves of the text and dropped the
 * middle, with nothing anywhere on the chat surface able to open it again.
 * Still not ported: the References accordion, which was not part of this
 * round's scope.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { ApplicationAnswerActions } from './ApplicationAnswerActions';
import { AssistantAvatar } from './MessageAvatar';
import { MessageHeaderRow } from './MessageHeaderRow';
import { actionKey, asDraft, ApplicationAnswerThinking, swarmChildContent } from './ApplicationAnswerThinking';
import { ChatContinue } from '../chat-continue/ChatContinue';
import type { McpAuthRequiredAction } from '../chat-continue/ChatContinue';
import { ChatHitlActions } from '../chat-hitl-actions/ChatHitlActions';
import type { HitlInterrupt, HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import { ErrorTrace } from '../error-trace/ErrorTrace';

import { AnswerMessageItems, readAnswerItems } from './AnswerMessageItems';
import type { CanvasEditPayload, CodeBlockInfo } from '../canvas/Canvas';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { Markdown } from '@/shared/ui/Markdown';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/** Loading/streaming/regenerating status flags, grouped to stay under the component-props budget. */
export interface ApplicationAnswerStatus {
  readonly isLoading?: boolean;
  readonly isStreaming?: boolean;
  readonly isRegenerating?: boolean;
}

/** Copy/delete/regenerate action handlers, grouped to stay under the component-props budget. */
export interface ApplicationAnswerActionHandlers {
  readonly onCopy?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  readonly onRegenerate?: (() => void) | undefined;
  readonly shouldDisableRegenerate?: boolean;
  /**
   * Opens the canvas editor for a `canvas_message` item in this answer — the
   * opener issue 853 is about. It rides in this group rather than as its own
   * prop because the component is at its §3.5 props ceiling, and it belongs
   * with the other per-message actions.
   */
  readonly onEditCanvas?: ((payload: CanvasEditPayload) => void) | undefined;
  /** The canvas block currently open in the editor, so this answer's copy of it shows a placeholder instead. */
  readonly selectedCodeBlockInfo?: CodeBlockInfo | undefined;
}

/** Read-aloud (TTS) props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerTts {
  readonly onAutoSpeak?: ((text: string, messageId: string) => void) | undefined;
  readonly speakingMessageId?: string;
  /** Not yet consumed, see module doc. */
  readonly speakingSegments?: readonly unknown[];
  /** The word being read aloud, as an offset range — see module doc for the `speakingMessageId` gate. */
  readonly spokenRange?: { readonly start: number; readonly end: number };
}

/** MCP-auth / token-limit continue-execution props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerContinuation {
  readonly onContinueMcpExecution?: ((messageId: string, addToIgnoreList?: boolean) => void) | undefined;
  readonly onContinueTokenLimitExecution?: ((messageId: string) => void) | undefined;
  readonly hideContinueButton?: boolean;
}

/** HITL interrupt/resume props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerHitl {
  /** HITL interrupt for resume (single-pause shape). */
  readonly hitlInterrupt?: unknown;
  /** HITL interrupts for resume (parallel-fan-out shape). */
  readonly hitlInterrupts?: readonly unknown[] | undefined;
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
}

/** Caption-line identity: who answered, and whether this row is a sub-agent's. Grouped to stay under the §3.5 component-props budget. */
export interface ApplicationAnswerAuthor {
  /**
   * The answering participant's display name, shown in the caption line
   * (`<mark> Elitea to Message`). Supplied by the list, which is where the
   * conversation's participants are known — `entities/message`'s assistant
   * normaliser drops the participant, so the row cannot resolve it alone.
   */
  readonly participantName?: string | undefined;
  /** Whether this is a swarm child message. */
  readonly isSwarmChild?: boolean;
  /** Display name of the swarm agent. */
  readonly swarmAgentName?: string;
}

/** @public Props for `ApplicationAnswer`. */
export interface ApplicationAnswerProps {
  /** The AI answer message to render. */
  readonly answer: ChatMessage;
  /** Message ID for tracking. */
  readonly messageId: string;
  /** Tool actions for this answer (thinking steps, tool calls, swarm children). */
  readonly toolActions?: readonly SubAgentGroupable[] | undefined;
  /** Whether auto-speak mode is active. */
  readonly isSpeakingMode?: boolean;
  /** Whether this is the last message. */
  readonly isLastMessage?: boolean;
  /** Who the row is captioned as, grouped to stay under the component-props budget. */
  readonly author?: ApplicationAnswerAuthor;
  readonly status?: ApplicationAnswerStatus;
  readonly actions?: ApplicationAnswerActionHandlers;
  readonly tts?: ApplicationAnswerTts;
  readonly continuation?: ApplicationAnswerContinuation;
  readonly hitl?: ApplicationAnswerHitl;
}

/** Defensive read of a message-level token-limit pause signal — not yet a typed `ChatMessage` field (see module doc). */
function getRequiresConfirmation(answer: ChatMessage): { readonly message?: string } | undefined {
  return (answer as unknown as { requiresConfirmation?: { readonly message?: string } }).requiresConfirmation;
}

/**
 * `ApplicationAnswer` — renders an AI assistant's answer with markdown
 * content, tool actions (accordion), error traces, HITL/continue prompts,
 * and action buttons.
 */
// eslint-disable-next-line eslint/complexity, eslint/max-lines-per-function -- integrates markdown/tool-actions/error-trace/HITL/continue/buttons; oxlint's complexity+max-lines are already disabled repo-wide for this directory (.oxlintrc.json)
export function ApplicationAnswer({
  answer,
  messageId,
  toolActions = [],
  isSpeakingMode = false,
  isLastMessage = false,
  author: { participantName, isSwarmChild = false, swarmAgentName = '' } = {},
  status: { isLoading = false, isStreaming = false, isRegenerating = false } = {},
  actions: { onCopy, onDelete, onRegenerate, shouldDisableRegenerate = false, onEditCanvas, selectedCodeBlockInfo } = {},
  tts: { onAutoSpeak, speakingMessageId, spokenRange } = {},
  continuation: { onContinueMcpExecution, onContinueTokenLimitExecution, hideContinueButton = false } = {},
  hitl: { hitlInterrupt, hitlInterrupts, onHitlResume } = {},
}: ApplicationAnswerProps): ReactNode {
  const isProcessing = isLoading || isRegenerating || isStreaming;
  const isLoadingOrRegenerating = isLoading || isRegenerating;
  const exception = answer.exception;
  const canRenderContent = !isLoadingOrRegenerating;
  // `spokenRange` is one global range, not scoped to a message id — it only
  // means something for the row TTS is CURRENTLY reading. Every other answer
  // row renders the same prop and must not highlight an unrelated offset.
  const currentSpokenRange = speakingMessageId === messageId ? spokenRange : undefined;
  const requiresConfirmationSignal = getRequiresConfirmation(answer);

  const items = useMemo(() => readAnswerItems(answer.messageItems), [answer.messageItems]);
  /*
   * `answer.content` and the TEXT items are two spellings of the same words,
   * and which arrives depends on the read: the paginated messages route
   * collapses a group's text into `content` and serves no text item, the
   * details read serves the items. So `content` is rendered only when no TEXT
   * item states it — gating that on `items.length` (canvases included) drops
   * the words of an answer whose only item on that route is its canvas. The
   * canvas itself still counts as content, or an answer that is nothing but a
   * canvas would render as an empty bubble.
   */
  const hasTextItems = useMemo(() => items.some((item) => item.kind === 'text'), [items]);
  const hasTextContent = !!answer.content || items.length > 0;

  const { swarmChildActions, nonSwarmChildActions } = useMemo(() => {
    if (isProcessing) return { swarmChildActions: [] as readonly SubAgentGroupable[], nonSwarmChildActions: toolActions };
    const swarm = toolActions.filter((action) => asDraft(action).type === TOOL_ACTION_TYPES.SwarmChild);
    const others = toolActions.filter((action) => asDraft(action).type !== TOOL_ACTION_TYPES.SwarmChild);
    return { swarmChildActions: swarm, nonSwarmChildActions: others };
  }, [toolActions, isProcessing]);

  const authRequiredAction = useMemo(
    () => toolActions.find((action) => asDraft(action).status === ToolActionStatus.actionRequired),
    [toolActions],
  );

  const effectiveHitlInterrupts = useMemo<readonly HitlInterrupt[]>(() => {
    const list = Array.isArray(hitlInterrupts) && hitlInterrupts.length > 0 ? hitlInterrupts : hitlInterrupt ? [hitlInterrupt] : [];
    return list as unknown as readonly HitlInterrupt[];
  }, [hitlInterrupt, hitlInterrupts]);

  const onContinueWithoutAuth = useCallback(() => {
    onContinueMcpExecution?.(messageId, true);
  }, [onContinueMcpExecution, messageId]);

  const onAuthSuccess = useCallback(() => {
    onContinueMcpExecution?.(messageId, false);
  }, [onContinueMcpExecution, messageId]);

  const onContinueWithConfirmation = useCallback(() => {
    onContinueTokenLimitExecution?.(messageId);
  }, [onContinueTokenLimitExecution, messageId]);

  const handleAutoSpeak = useCallback(() => {
    onAutoSpeak?.(answer.content, messageId);
  }, [onAutoSpeak, answer.content, messageId]);

  // Auto-speak AI response in speaking mode when streaming/loading ends (last message only) — baseline: ApplicationAnswer.jsx:202-208.
  useEffect(() => {
    if (isSpeakingMode && isLastMessage && !isStreaming && !isLoading && hasTextContent) {
      onAutoSpeak?.(answer.content, messageId);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fire only on the streaming/loading transition, matching baseline
  }, [isStreaming, isLoading]);

  const shouldRenderAnswerBlock =
    hasTextContent ||
    !!exception ||
    (!!authRequiredAction && !!onContinueMcpExecution) ||
    (!!requiresConfirmationSignal && !!onContinueTokenLimitExecution) ||
    effectiveHitlInterrupts.length > 0;

  return (
    <Box
      data-testid="application-answer"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        alignSelf: 'stretch',
        width: '100%',
        // baseline `applicationAnswerStyles.userMessageContainer` (vertical):
        // `padding: 0.75rem 0; gap: 0.5rem`. Measured identically on the
        // production transcript's own `<li>`.
        gap: '0.5rem',
        padding: '0.75rem 0',
        borderRadius: '0.25rem',
        ...(isSwarmChild
          ? { ml: 6, pl: 2, borderLeft: '3px solid', borderColor: 'primary.main' }
          : {}),
      }}
    >
      {isSwarmChild && swarmAgentName ? (
        <Typography variant="caption" sx={{ color: 'text.secondary' }}>
          {swarmAgentName}
        </Typography>
      ) : (
        <MessageHeaderRow
          avatar={<AssistantAvatar />}
          name={participantName ?? ''}
          sentToName={t('features.chatMessages.replyTo', 'Message')}
          sentToInteractive
          createdAt={answer.createdAt}
        />
      )}

      {nonSwarmChildActions.length > 0 && <ApplicationAnswerThinking actions={nonSwarmChildActions} isStreaming={isProcessing} />}

      {!isProcessing && swarmChildActions.length > 0 && (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, mb: 0.5 }}>
          {swarmChildActions.map((action, index) => (
            <BasicAccordion
              key={actionKey(action, index)}
              defaultExpanded={false}
              items={[
                {
                  title: asDraft(action).name || 'Sub-agent',
                  content: <Markdown>{swarmChildContent(action)}</Markdown>,
                },
              ]}
            />
          ))}
        </Box>
      )}

      {shouldRenderAnswerBlock && (
        <Box
          data-testid={isLastMessage ? 'skill-test-last-response' : 'chat-answer-content'}
          sx={(theme) => ({
            // baseline `applicationAnswerStyles.answerBlock`, confirmed against
            // the live production row: `12px 16px` padding (not a uniform 12),
            // an 8px radius, a 3rem floor so a one-line answer keeps the same
            // card height, and the 0.5rem gap under the thinking accordion.
            width: '100%',
            boxSizing: 'border-box',
            backgroundColor: theme.vars.palette.background.aiAnswerBkg,
            color: theme.vars.palette.text.secondary,
            boxShadow: theme.vars.palette.boxShadow.aiAnswer,
            borderRadius: theme.vars.shape.radiusMd,
            padding: '0.75rem 1rem',
            minHeight: '3rem',
            position: 'relative',
            marginTop: nonSwarmChildActions.length > 0 || !!exception ? '0.5rem' : 0,
          })}
        >
          {canRenderContent && !!answer.content && !hasTextItems && (
            <Markdown spokenRange={currentSpokenRange}>{answer.content}</Markdown>
          )}

          {canRenderContent && items.length > 0 && (
            <AnswerMessageItems
              items={items}
              isStreaming={isStreaming}
              onEditCanvas={onEditCanvas}
              selectedCodeBlockInfo={selectedCodeBlockInfo}
              spokenRange={currentSpokenRange}
            />
          )}

          {!!exception && <ErrorTrace error={exception} />}

          {!!authRequiredAction && (
            <ChatContinue
              authRequired
              disabled={!onContinueMcpExecution}
              onContinueWithoutAuth={onContinueWithoutAuth}
              onAuthSuccess={onAuthSuccess}
              authRequiredAction={authRequiredAction as unknown as McpAuthRequiredAction}
            />
          )}

          {!hideContinueButton && !!requiresConfirmationSignal && (
            <ChatContinue
              requiresConfirmation
              disabled={!onContinueTokenLimitExecution}
              onContinue={onContinueWithConfirmation}
            />
          )}

          {effectiveHitlInterrupts.map((interrupt, index) => (
            <ChatHitlActions
              key={interrupt.tool_call_id || `hitl-${index}`}
              hitlInterrupt={interrupt}
              toolCallId={interrupt.tool_call_id ?? ''}
              onHitlResume={onHitlResume}
              disabled={!onHitlResume || Boolean(interrupt.decided)}
            />
          ))}

          {isLoadingOrRegenerating &&
            !hasTextContent &&
            !exception &&
            nonSwarmChildActions.length === 0 &&
            effectiveHitlInterrupts.length === 0 && (
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                <Box
                  component="span"
                  sx={{
                    display: 'inline-block',
                    width: '6px',
                    height: '6px',
                    borderRadius: '50%',
                    backgroundColor: 'primary.main',
                    animation: 'pulse 1.5s infinite',
                  }}
                />
                <Typography variant="body2" sx={{ color: 'text.secondary' }}>
                  {isStreaming ? 'Streaming...' : 'Loading...'}
                </Typography>
              </Box>
            )}

          <ApplicationAnswerActions
            hasContent={hasTextContent || !!exception}
            isProcessing={isProcessing}
            shouldDisableRegenerate={shouldDisableRegenerate}
            hasSpeakableText={hasTextContent}
            isSpeaking={!!speakingMessageId}
            onAutoSpeak={onAutoSpeak ? handleAutoSpeak : undefined}
            onCopy={onCopy}
            onRegenerate={onRegenerate}
            onDelete={onDelete}
          />
        </Box>
      )}
    </Box>
  );
}
