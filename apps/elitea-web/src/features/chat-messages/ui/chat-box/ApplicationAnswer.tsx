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
 * TTS highlighting applies only to `tts.speakingMessageId` (#625).
 * Other rows receive no range: the global offset is not scoped to a message.
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
import { MessageFeedbackControl } from './MessageFeedbackControl';
import { RememberMemoryAction } from './RememberMemoryAction';
import { MessageHeaderRow } from './MessageHeaderRow';
import { actionKey, asDraft, ApplicationAnswerThinking, swarmChildContent } from './ApplicationAnswerThinking';
import { ChatContinue } from '../chat-continue/ChatContinue';
import type { McpAuthRequiredAction } from '../chat-continue/ChatContinue';
import { ChatHitlActions } from '../chat-hitl-actions/ChatHitlActions';
import type { HitlInterrupt } from '../chat-hitl-actions/ChatHitlActions';
import { ErrorTrace } from '../error-trace/ErrorTrace';

import { AnswerContent } from './AnswerContent';
import { readAnswerItems } from './AnswerMessageItems';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { Markdown } from '@/shared/ui/Markdown';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

import type { ApplicationAnswerProps } from './ApplicationAnswer.types';

// `ApplicationAnswerProps` and the per-group prop interfaces it composes
// (`ApplicationAnswerStatus`/`ActionHandlers`/`Tts`/`Continuation`/
// `Feedback`/`Hitl`/`Author`) live in `./ApplicationAnswer.types` — split
// out purely to keep this file under the §3.5 file-length budget, same
// rationale as `NewChatInput.types.ts`. `features/chat-messages/index.ts`
// imports `ApplicationAnswerProps` directly from `./ApplicationAnswer.types`,
// not through this re-export, so there is no separate barrel entry to keep
// in sync here.
export type {
  ApplicationAnswerActionHandlers,
  ApplicationAnswerAuthor,
  ApplicationAnswerContinuation,
  ApplicationAnswerFeedback,
  ApplicationAnswerHitl,
  ApplicationAnswerProps,
  ApplicationAnswerStatus,
  ApplicationAnswerTts,
} from './ApplicationAnswer.types';

function authorizationRequestId(action: SubAgentGroupable): string | undefined {
  const draft = asDraft(action);
  const metadata = (draft.toolMeta ?? {}) as Record<string, unknown>;
  const value = draft.authorizationRequestId ?? metadata['authorization_request_id'] ?? metadata['interrupt_id'] ?? draft.id;
  return typeof value === 'string' && value !== '' ? value : undefined;
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
  actions: { onCopy, onDelete, onRegenerate, shouldDisableRegenerate = false, onEditCanvas, selectedCodeBlockInfo, onCreateCanvasFromSelection } = {},
  tts: { onAutoSpeak, speakingMessageId, spokenRange } = {},
  continuation: { onContinueMcpExecution, onContinueTokenLimitExecution, renderAuthModal, hideContinueButton = false } = {},
  hitl: { hitlInterrupt, hitlInterrupts, onHitlResume } = {},
  feedback: { projectId: feedbackProjectId, enabled: feedbackEnabled = true } = {},
}: ApplicationAnswerProps): ReactNode {
  const isProcessing = isLoading || isRegenerating || isStreaming;
  const isLoadingOrRegenerating = isLoading || isRegenerating;
  const showFeedback = Boolean(feedbackProjectId) && feedbackEnabled && !isProcessing;
  const exception = answer.exception;
  const canRenderContent = !isLoadingOrRegenerating;
  // `spokenRange` is one global range, not scoped to a message id — it only
  // means something for the row TTS is CURRENTLY reading. Every other answer
  // row renders the same prop and must not highlight an unrelated offset.
  const currentSpokenRange = speakingMessageId === messageId ? spokenRange : undefined;
  const requiresConfirmationSignal = getRequiresConfirmation(answer);

  const items = useMemo(() => readAnswerItems(answer.messageItems), [answer.messageItems]);
  // A canvas counts as content: an answer that is nothing but a canvas would
  // otherwise render as an empty bubble. Which of `content` and the text items
  // actually carries the words is `./AnswerContent`'s subject.
  const hasTextContent = !!answer.content || items.length > 0;

  /*
   * "Open as document" (issue #879) — the WHOLE answer carved into a
   * `document` canvas, not a range the reader highlighted. Only offered
   * before the answer has been split at all: once a canvas already exists
   * inside it (`items.length > 1`, or the lone item is itself a canvas), the
   * words for a create() are not one contiguous range any more, and this
   * button would ask the create route to carve a "selection" that spans a
   * canvas block it cannot serialise as text.
   */
  const soleItem = items.length === 1 ? items[0] : undefined;
  const wholeAnswerText = useMemo(() => {
    if (items.length > 1) return undefined;
    if (soleItem !== undefined) return soleItem.kind === 'text' ? soleItem.content : undefined;
    return answer.content;
  }, [items.length, soleItem, answer.content]);
  const wholeAnswerItemId = soleItem?.kind === 'text' ? soleItem.messageItemId : undefined;

  const onOpenAsDocument = useCallback(() => {
    const text = wholeAnswerText;
    if (!onCreateCanvasFromSelection || !text || text.trim() === '') return;
    onCreateCanvasFromSelection({
      messageGroupUuid: answer.id,
      selectedText: text,
      messageItemId: wholeAnswerItemId,
      kind: 'document',
    });
  }, [onCreateCanvasFromSelection, wholeAnswerText, wholeAnswerItemId, answer.id]);

  const { swarmChildActions, nonSwarmChildActions } = useMemo(() => {
    if (isProcessing) return { swarmChildActions: [] as readonly SubAgentGroupable[], nonSwarmChildActions: toolActions };
    const swarm = toolActions.filter((action) => asDraft(action).type === TOOL_ACTION_TYPES.SwarmChild);
    const others = toolActions.filter((action) => asDraft(action).type !== TOOL_ACTION_TYPES.SwarmChild);
    return { swarmChildActions: swarm, nonSwarmChildActions: others };
  }, [toolActions, isProcessing]);

  const authRequiredActions = useMemo(
    () => toolActions.filter((action) => asDraft(action).status === ToolActionStatus.actionRequired),
    [toolActions],
  );

  const effectiveHitlInterrupts = useMemo<readonly HitlInterrupt[]>(() => {
    const list = Array.isArray(hitlInterrupts) && hitlInterrupts.length > 0 ? hitlInterrupts : hitlInterrupt ? [hitlInterrupt] : [];
    return list as unknown as readonly HitlInterrupt[];
  }, [hitlInterrupt, hitlInterrupts]);

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
    (authRequiredActions.length > 0 && !!onContinueMcpExecution) ||
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
          memoriesUsed={answer.memoriesUsed}
        />
      )}

      {authRequiredActions.map((action, index) => {
        const requestId = authorizationRequestId(action);
        const owner = asDraft(action).parent_agent_name || asDraft(action).name || 'Toolkit';
        return (
          <Box key={requestId ?? `authorization-${index}`} component="section"
            aria-label={`${owner} authorization`} sx={{ p: 1.5, border: 1, borderColor: 'warning.main' }}>
            <Typography variant="subtitle2">{owner} — Authorization required</Typography>
            <Typography variant="body2">Execution is paused. The protected tool has not run.</Typography>
            <ChatContinue
              authRequired
              disabled={!onContinueMcpExecution || !requestId}
              onContinueWithoutAuth={() => { onContinueMcpExecution?.(messageId, true, requestId); }}
              onAuthSuccess={() => { onContinueMcpExecution?.(messageId, false, requestId); }}
              authRequiredAction={action as unknown as McpAuthRequiredAction}
              renderAuthModal={renderAuthModal}
            />
          </Box>
        );
      })}

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
          {canRenderContent && (
            <AnswerContent
              content={answer.content}
              items={items}
              messageGroupUuid={answer.id}
              isStreaming={isStreaming}
              spokenRange={currentSpokenRange}
              onEditCanvas={onEditCanvas}
              selectedCodeBlockInfo={selectedCodeBlockInfo}
              onCreateCanvasFromSelection={onCreateCanvasFromSelection}
            />
          )}

          {!!exception && <ErrorTrace error={exception} />}

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

          <Box
            sx={{
              display: 'flex',
              // `space-between` needs TWO items to place one at each edge — a
              // single flex child under it sits at flex-start, which would
              // silently un-right-align the actions row on every message with
              // no resolved project id. `flex-end` is the ORIGINAL single-row
              // alignment (`ApplicationAnswerActions`'s own box), kept as the
              // fallback rather than assumed to still hold once this became a
              // two-item row.
              justifyContent: showFeedback ? 'space-between' : 'flex-end',
              alignItems: 'flex-start',
            }}
          >
            {showFeedback && <MessageFeedbackControl projectId={feedbackProjectId as string} messageId={messageId} />}
            {showFeedback && (
              // #870 "Remember this" — same gate as the feedback control
              // beside it (a resolved project id, not still processing).
              // `conversationId` is not yet threaded to this row (no
              // `ChatMessage` field carries it today), so a memory saved
              // here has no `source_conversation_id` — informational-only
              // provenance, not a functional gap.
              <RememberMemoryAction projectId={feedbackProjectId as string} content={answer.content} disabled={isProcessing} />
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
              onOpenAsDocument={
                onCreateCanvasFromSelection && wholeAnswerText && wholeAnswerText.trim() !== '' ? onOpenAsDocument : undefined
              }
            />
          </Box>
        </Box>
      )}
    </Box>
  );
}
