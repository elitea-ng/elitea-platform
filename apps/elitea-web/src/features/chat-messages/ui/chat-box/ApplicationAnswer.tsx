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
 * Not ported: per-word TTS highlight sync (`spokenRange` translated into the
 * rendered markdown) — `shared/ui/Markdown` has no `spokenRange` prop yet,
 * unlike the baseline's richer markdown renderer; the read-aloud button
 * itself (`onAutoSpeak`) is fully wired. Canvas message items and the
 * References accordion are also not rendered — `entities/message`'s wire
 * type does not model canvas fields yet, and References wasn't part of this
 * fix round's scope.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { ApplicationAnswerActions } from './ApplicationAnswerActions';
import { actionKey, asDraft, ApplicationAnswerThinking, swarmChildContent } from './ApplicationAnswerThinking';
import { ChatContinue } from '../chat-continue/ChatContinue';
import type { ChatContinueProps, McpAuthRequiredAction } from '../chat-continue/ChatContinue';
import { ChatHitlActions } from '../chat-hitl-actions/ChatHitlActions';
import type { HitlInterrupt, HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import { ErrorTrace } from '../error-trace/ErrorTrace';

import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { Markdown } from '@/shared/ui/Markdown';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

const TEXT_MESSAGE_ITEM_TYPE = 'text_message';

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
}

/** Read-aloud (TTS) props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerTts {
  readonly onAutoSpeak?: ((text: string, messageId: string) => void) | undefined;
  readonly speakingMessageId?: string;
  /** Not yet consumed, see module doc. */
  readonly speakingSegments?: readonly unknown[];
  /** Not yet consumed, see module doc. */
  readonly spokenRange?: { readonly start: number; readonly end: number };
}

/** MCP-auth / token-limit continue-execution props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerContinuation {
  readonly onContinueMcpExecution?: ((messageId: string, addToIgnoreList?: boolean, authorizationRequestId?: string) => void) | undefined;
  readonly onContinueTokenLimitExecution?: ((messageId: string) => void) | undefined;
  readonly renderAuthModal?: ChatContinueProps['renderAuthModal'];
  readonly hideContinueButton?: boolean;
}

function authorizationRequestId(action: SubAgentGroupable): string | undefined {
  const draft = asDraft(action);
  const metadata = (draft.toolMeta ?? {}) as Record<string, unknown>;
  const value = draft.authorizationRequestId ?? metadata['authorization_request_id'] ?? metadata['interrupt_id'] ?? draft.id;
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/** HITL interrupt/resume props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerHitl {
  /** HITL interrupt for resume (single-pause shape). */
  readonly hitlInterrupt?: unknown;
  /** HITL interrupts for resume (parallel-fan-out shape). */
  readonly hitlInterrupts?: readonly unknown[] | undefined;
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
}

/** @public Props for `ApplicationAnswer`. */
export interface ApplicationAnswerProps {
  /** The AI answer message to render. */
  readonly answer: ChatMessage;
  /** Message ID for tracking. */
  readonly messageId: string;
  /** Tool actions for this answer (thinking steps, tool calls, swarm children). */
  readonly toolActions?: readonly SubAgentGroupable[] | undefined;
  /** Whether this is a swarm child message. */
  readonly isSwarmChild?: boolean;
  /** Display name of the swarm agent. */
  readonly swarmAgentName?: string;
  /** Whether auto-speak mode is active. */
  readonly isSpeakingMode?: boolean;
  /** Whether this is the last message. */
  readonly isLastMessage?: boolean;
  readonly status?: ApplicationAnswerStatus;
  readonly actions?: ApplicationAnswerActionHandlers;
  readonly tts?: ApplicationAnswerTts;
  readonly continuation?: ApplicationAnswerContinuation;
  readonly hitl?: ApplicationAnswerHitl;
}

/** The `text_message` entries of `messageItems`, defensively read the same way `UserMessage.tsx` reads them (the declared `MessageItemWire` type only models `id`/`item_details.content`). */
function getTextMessageItems(
  messageItems: ChatMessage['messageItems'],
): ReadonlyArray<{ readonly key: string; readonly content: string }> {
  const raw = (messageItems ?? []) as unknown as ReadonlyArray<Record<string, unknown>>;
  return raw
    .filter((item) => item.item_type === TEXT_MESSAGE_ITEM_TYPE)
    .map((item, index) => {
      const details = item.item_details as { content?: string } | undefined;
      const uuid = item.uuid as string | undefined;
      return { key: uuid ?? `text-item-${index}`, content: details?.content ?? '' };
    });
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
  isSwarmChild = false,
  swarmAgentName = '',
  isSpeakingMode = false,
  isLastMessage = false,
  status: { isLoading = false, isStreaming = false, isRegenerating = false } = {},
  actions: { onCopy, onDelete, onRegenerate, shouldDisableRegenerate = false } = {},
  tts: { onAutoSpeak, speakingMessageId } = {},
  continuation: { onContinueMcpExecution, onContinueTokenLimitExecution, renderAuthModal, hideContinueButton = false } = {},
  hitl: { hitlInterrupt, hitlInterrupts, onHitlResume } = {},
}: ApplicationAnswerProps): ReactNode {
  const isProcessing = isLoading || isRegenerating || isStreaming;
  const isLoadingOrRegenerating = isLoading || isRegenerating;
  const exception = answer.exception;
  const canRenderContent = !isLoadingOrRegenerating;
  const requiresConfirmationSignal = getRequiresConfirmation(answer);

  const textItems = useMemo(() => getTextMessageItems(answer.messageItems), [answer.messageItems]);
  const hasTextContent = !!answer.content || textItems.length > 0;

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
        gap: 0.5,
        mb: 1,
        ...(isSwarmChild
          ? { ml: 6, pl: 2, borderLeft: '3px solid', borderColor: 'primary.main' }
          : {}),
      }}
    >
      {isSwarmChild && swarmAgentName && (
        <Typography variant="caption" sx={{ color: 'text.secondary' }}>
          {swarmAgentName}
        </Typography>
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

      {nonSwarmChildActions.length > 0 && <ApplicationAnswerThinking actions={nonSwarmChildActions} />}

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
            backgroundColor: theme.vars.palette.background.aiAnswerBkg,
            borderRadius: theme.vars.shape.radiusMd,
            p: 1.5,
            '&:hover .actionButtons': { visibility: 'visible' },
          })}
        >
          {canRenderContent && !!answer.content && textItems.length === 0 && <Markdown>{answer.content}</Markdown>}

          {canRenderContent && textItems.map((item) => <Markdown key={item.key}>{item.content}</Markdown>)}

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
