/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * ChatHitlActions.jsx` — the inline card a paused agent run waits on: a
 * generic approval card, a sensitive-tool authorization card, or the
 * clarifying-question controls of an `ask_user` pause.
 *
 * Every branch keeps an outer `data-testid="chat-hitl-actions"` wrapper
 * (`display: contents`, so it adds no box) for the existing e2e locators; the
 * baseline's own test ids sit on the visible panel inside it.
 */
import type { ReactNode } from 'react';
import { useId, useState } from 'react';

import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';
import { RejectIcon } from '@/shared/ui/icons/reject-icon';
import { Markdown } from '@/shared/ui/Markdown';

import type { HitlQuestion } from '../../lib/hitlInterrupts';

import { AnswerQuestionsControl } from './AnswerQuestionsControl';
import { BlockWithCommentControl } from './BlockWithCommentControl';
import { EditControl } from './EditControl';

const SENSITIVE_PARAM_MASK = '***';
const SENSITIVE_GUARDRAIL_TYPES = new Set(['sensitive_tool', 'parallel_sensitive_tools']);
const ANSWER_ACTION = 'answer';
const BLOCK_WITH_COMMENT_ACTION = 'block_with_comment';

/**
 * @public The payload `ChatHitlActions` resumes a paused agent with.
 *
 * `answer` carries the clarification answers as a JSON-encoded `value` — the
 * ONE action whose value is not free prose. `buildHitlContinueBody` parses it
 * back into the structured `hitl_value` the continuation route canonicalises
 * (`currentHITLValue`, agentexecution/route.go).
 */
export interface HitlResumePayload {
  readonly action: 'approve' | 'reject' | 'edit' | 'block_with_comment' | 'answer';
  readonly value?: string | undefined;
  readonly toolCallId?: string | undefined;
  readonly interruptId?: string | undefined;
}

/** The HITL interrupt data shared by every `ChatHitlActions` branch. */
export interface HitlInterrupt {
  readonly message?: string;
  readonly tool_name?: string;
  readonly toolkit_name?: string;
  readonly available_actions?: readonly string[];
  readonly decided?: boolean;
  readonly queued?: boolean;
  readonly hidden?: boolean;
  readonly tool_call_id?: string;
  readonly interrupt_id?: string;
  /** `sensitive_tool` / `parallel_sensitive_tools` switch to the authorization-card branch. */
  readonly guardrail_type?: string;
  /** The clarifying questions of an `ask_user` pause (`guardrail_type: 'clarifying_question'`). */
  readonly questions?: readonly HitlQuestion[];
  readonly action_label?: string;
  readonly tool_args?: unknown;
  readonly policy_message?: string;
}

/** @public Props for `ChatHitlActions`. */
export interface ChatHitlActionsProps {
  readonly hitlInterrupt: HitlInterrupt;
  /** The tool call id the resume is routed by. */
  readonly toolCallId?: string;
  /** The interrupt id the resume is routed by, when the pause carries one. */
  readonly interruptId?: string;
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
  /** Disables every control — e.g. once the pause is decided. Visibility is the parent's call. */
  readonly disabled?: boolean;
}

/**
 * Collapsible `tool_args` list. Values the server masked (the literal `'***'`)
 * are rendered as-is; everything else is stringified for display.
 */
function SensitiveToolParams({ toolArgs }: { readonly toolArgs: unknown }): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const contentId = useId();

  const paramEntries = toolArgs !== null && typeof toolArgs === 'object' ? Object.entries(toolArgs) : [];
  if (paramEntries.length === 0) return null;

  const toggle = (): void => setExpanded((prev) => !prev);

  return (
    <Box sx={paramsWrapperSx}>
      <Box
        component="button"
        type="button"
        sx={paramsHeaderSx}
        onClick={toggle}
        aria-expanded={expanded}
        aria-controls={contentId}
      >
        <Typography variant="labelSmall" sx={paramKeySx}>
          {t('features.chatMessages.hitl.parameters', 'Parameters')} {expanded ? '▾' : '▸'}
        </Typography>
      </Box>
      <Collapse in={expanded} id={contentId}>
        <Box sx={paramListSx}>
          {paramEntries.map(([key, value]) => (
            <Box key={key} sx={paramRowSx}>
              <Typography variant="labelSmall" sx={paramKeySx}>
                {key}:
              </Typography>
              <Typography variant="labelSmall" sx={paramValueSx}>
                {value === SENSITIVE_PARAM_MASK
                  ? SENSITIVE_PARAM_MASK
                  : String(typeof value === 'object' ? JSON.stringify(value) : value)}
              </Typography>
            </Box>
          ))}
        </Box>
      </Collapse>
    </Box>
  );
}

interface CardHandlers {
  readonly disabled: boolean;
  readonly onApprove: () => void;
  readonly onReject: () => void;
  readonly onBlockWithComment: ((comment: string) => void) | undefined;
}

/** The sensitive-tool authorization card (`guardrail_type` branch). */
function SensitiveToolCard({
  hitlInterrupt,
  disabled,
  onApprove,
  onReject,
  onBlockWithComment,
}: CardHandlers & { readonly hitlInterrupt: HitlInterrupt }): ReactNode {
  return (
    <Box data-testid="sensitive-action-panel" sx={sensitiveContainerSx}>
      <Typography variant="headingSmall" sx={sensitiveTitleSx}>
        {t('features.chatMessages.hitl.sensitiveTitle', '⚠️ Sensitive Action Authorization Required')}
      </Typography>
      <Box sx={sensitiveActionBlockSx}>
        <Typography variant="bodySmall" sx={secondaryTextSx}>
          {t('features.chatMessages.hitl.aboutToPerform', 'Agent is about to perform:')}
        </Typography>
        <Typography variant="headingSmall" sx={primaryTextSx}>
          {hitlInterrupt.action_label ||
            hitlInterrupt.tool_name ||
            t('features.chatMessages.hitl.unknownAction', 'Unknown action')}
        </Typography>
      </Box>
      {Boolean(hitlInterrupt.tool_args) && <SensitiveToolParams toolArgs={hitlInterrupt.tool_args} />}
      {hitlInterrupt.policy_message && (
        <Typography variant="bodySmall" sx={sensitivePolicySx}>
          {hitlInterrupt.policy_message}
        </Typography>
      )}
      <Box sx={buttonContainerSx}>
        <BaseBtn
          data-testid="sensitive-action-authorize-button"
          variant="positive"
          startIcon={<CheckedIcon />}
          onClick={onApprove}
          disabled={disabled}
        >
          {t('features.chatMessages.hitl.authorize', 'Authorize')}
        </BaseBtn>
        <BaseBtn
          data-testid="sensitive-action-block-button"
          variant="alarm"
          startIcon={<RejectIcon />}
          onClick={onReject}
          disabled={disabled}
        >
          {t('features.chatMessages.hitl.block', 'Block')}
        </BaseBtn>
        {onBlockWithComment && (
          <BlockWithCommentControl
            onSubmit={onBlockWithComment}
            disabled={disabled}
            triggerTestId="sensitive-action-block-with-comment-button"
            textareaTestId="sensitive-action-block-comment-input"
            submitTestId="sensitive-action-block-comment-submit-button"
          />
        )}
      </Box>
    </Box>
  );
}

/** The generic approval card: Approve, Edit, Reject, Block with Comment — the baseline's order. */
function GenericHitlCard({
  hitlInterrupt,
  actions,
  disabled,
  onApprove,
  onReject,
  onBlockWithComment,
  onEdit,
}: CardHandlers & {
  readonly hitlInterrupt: HitlInterrupt;
  readonly actions: readonly string[];
  readonly onEdit: (value: string) => void;
}): ReactNode {
  const message = hitlInterrupt.message?.trim()
    ? hitlInterrupt.message
    : t('features.chatMessages.hitl.defaultMessage', 'Here are some results. Choose the action to proceed.');
  return (
    <Box data-testid="chat-hitl-actions-panel" sx={containerSx}>
      <Markdown sx={messageSx} renderHtml={false}>{message}</Markdown>
      <Box sx={buttonContainerSx}>
        {actions.includes('approve') && (
          <BaseBtn
            data-testid="chat-hitl-approve-button"
            variant="positive"
            startIcon={<CheckedIcon />}
            onClick={onApprove}
            disabled={disabled}
          >
            {t('features.chatMessages.hitl.approve', 'Approve')}
          </BaseBtn>
        )}
        {actions.includes('edit') && <EditControl onSubmit={onEdit} disabled={disabled} />}
        {actions.includes('reject') && (
          <BaseBtn
            data-testid="chat-hitl-reject-button"
            variant="alarm"
            startIcon={<RejectIcon />}
            onClick={onReject}
            disabled={disabled}
          >
            {t('features.chatMessages.hitl.reject', 'Reject')}
          </BaseBtn>
        )}
        {onBlockWithComment && <BlockWithCommentControl onSubmit={onBlockWithComment} disabled={disabled} />}
      </Box>
    </Box>
  );
}

/** `ChatHitlActions` — the inline card for a paused agent run. */
export function ChatHitlActions({
  hitlInterrupt,
  toolCallId,
  interruptId,
  onHitlResume,
  disabled = false,
}: ChatHitlActionsProps): ReactNode {
  if (!hitlInterrupt) return null;

  const actions = hitlInterrupt.available_actions ?? ['approve', 'reject'];
  const resume = (action: HitlResumePayload['action'], value?: string): void =>
    onHitlResume?.({ action, ...(value === undefined ? {} : { value }), toolCallId, interruptId });
  const handleApprove = (): void => resume('approve');
  const handleReject = (): void => resume('reject');
  /**
   * #950 (ELITEA-1013): `reject` refuses a value and `block_with_comment`
   * REQUIRES one (`agentexecution/continue.go`'s `validCurrentHITLDecision`),
   * so a blank comment is sent as the plain reject it is. A comment travels
   * unchanged (not trimmed) whenever there is one.
   */
  const handleBlockWithComment = actions.includes(BLOCK_WITH_COMMENT_ACTION)
    ? (comment: string): void => {
        if (comment.trim() === '' && actions.includes('reject')) {
          handleReject();
          return;
        }
        resume('block_with_comment', comment);
      }
    : undefined;

  let card: ReactNode;
  if (actions.includes(ANSWER_ACTION) || hitlInterrupt.guardrail_type === 'clarifying_question') {
    card = (
      <Box sx={clarifyingContainerSx}>
        {hitlInterrupt.message?.trim() && <Markdown sx={messageSx} renderHtml={false}>{hitlInterrupt.message}</Markdown>}
        <AnswerQuestionsControl
          questions={hitlInterrupt.questions ?? []}
          disabled={disabled}
          onSubmit={(value) => resume('answer', value)}
        />
      </Box>
    );
  } else if (hitlInterrupt.guardrail_type !== undefined && SENSITIVE_GUARDRAIL_TYPES.has(hitlInterrupt.guardrail_type)) {
    card = (
      <SensitiveToolCard
        hitlInterrupt={hitlInterrupt}
        disabled={disabled}
        onApprove={handleApprove}
        onReject={handleReject}
        onBlockWithComment={handleBlockWithComment}
      />
    );
  } else {
    card = (
      <GenericHitlCard
        hitlInterrupt={hitlInterrupt}
        actions={actions}
        disabled={disabled}
        onApprove={handleApprove}
        onReject={handleReject}
        onBlockWithComment={handleBlockWithComment}
        onEdit={(value) => resume('edit', value)}
      />
    );
  }

  return (
    <Box data-testid="chat-hitl-actions" sx={{ display: 'contents' }}>
      {card}
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  padding: '0.875rem 1rem 0.9375rem',
  gap: '0.75rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.chatContinueBackground,
  alignItems: 'flex-start',
});

const clarifyingContainerSx: SxProps<Theme> = {
  width: '100%', display: 'flex', flexDirection: 'column', gap: '0.75rem', alignItems: 'flex-start',
};

const sensitiveContainerSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  padding: '0.625rem 0.75rem',
  gap: '0.375rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.chatContinueBackground,
  border: `0.0625rem solid ${theme.vars.palette.warning.main}`,
  alignItems: 'flex-start',
});

const sensitiveTitleSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.warning.main });
const sensitiveActionBlockSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.125rem' };
const secondaryTextSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
const primaryTextSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.primary });
const sensitivePolicySx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary, fontStyle: 'italic' });
const messageSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
const buttonContainerSx: SxProps<Theme> = { display: 'flex', gap: '0.625rem', alignItems: 'center', flexWrap: 'wrap' };

const paramsWrapperSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'hidden',
});
const paramsHeaderSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  border: 'none',
  textAlign: 'left',
  font: 'inherit',
  display: 'flex',
  alignItems: 'center',
  padding: '0.375rem 0.625rem',
  cursor: 'pointer',
  userSelect: 'none',
  backgroundColor: theme.vars.palette.background.aiProviderAccordion.default,
  '&:hover': { backgroundColor: theme.vars.palette.background.aiProviderAccordion.hover },
});
const paramListSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.125rem', padding: '0.375rem 0.625rem' };
const paramRowSx: SxProps<Theme> = { display: 'flex', gap: '0.5rem', alignItems: 'flex-start' };
const paramKeySx: SxProps<Theme> = (theme) => ({
  fontWeight: 600,
  color: theme.vars.palette.text.secondary,
  flexShrink: 0,
});
const paramValueSx: SxProps<Theme> = (theme) => ({
  fontWeight: 400,
  color: theme.vars.palette.text.primary,
  wordBreak: 'break-word',
});
