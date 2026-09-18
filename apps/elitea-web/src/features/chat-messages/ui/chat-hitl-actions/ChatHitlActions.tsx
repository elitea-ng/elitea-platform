/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * ChatHitlActions.jsx` — renders HITL (Human-In-The-Loop) approval controls
 * for paused agent execution.
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * ChatHitlActions.jsx`.
 */
import type { ReactNode } from 'react';
import { useId, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Collapse from '@mui/material/Collapse';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import type { HitlQuestion } from '../../lib/hitlInterrupts';

import { ClarifyingQuestionCard } from './AnswerQuestionsControl';
import { BlockWithCommentControl } from './BlockWithCommentControl';
import { EditControl } from './EditControl';

const SENSITIVE_PARAM_MASK = '***';

/** @public The `available_actions` guardrail flags a sensitive-tool pause. */
const SENSITIVE_GUARDRAIL_TYPES = new Set(['sensitive_tool', 'parallel_sensitive_tools']);

/** The `available_actions` entry an `ask_user` clarification pause carries. */
const ANSWER_ACTION = 'answer';

/**
 * @public The payload `ChatHitlActions` resumes a paused agent with.
 *
 * `answer` carries the clarification answers as a JSON-encoded `value` — the
 * ONE action whose value is not free prose. `buildHitlContinueBody` parses it
 * back into the structured `hitl_value` the continuation route canonicalises
 * (`currentHITLValue`, agentexecution/route.go); every layer between here and
 * there types the value as a string, so it travels as one.
 */
export interface HitlResumePayload {
  readonly action: 'approve' | 'reject' | 'edit' | 'block_with_comment' | 'answer';
  readonly value?: string | undefined;
  readonly toolCallId?: string | undefined;
}

/** The HITL interrupt data shared by both `ChatHitlActions` render branches. */
export interface HitlInterrupt {
  readonly message?: string;
  readonly tool_name?: string;
  readonly toolkit_name?: string;
  readonly available_actions?: readonly string[];
  readonly decided?: boolean;
  readonly tool_call_id?: string;
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
  /** The HITL interrupt data. */
  readonly hitlInterrupt: HitlInterrupt;
  /** The tool call ID for routing. */
  readonly toolCallId?: string;
  /** Called when HITL is resumed. */
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
  /** Whether the actions are disabled. */
  readonly disabled?: boolean;
}

interface SensitiveToolParamsProps {
  readonly toolArgs: unknown;
}

/**
 * Collapsible `tool_args` list for the sensitive-tool authorization card.
 * Values the caller already masked (server sends the literal `'***'`) are
 * rendered as-is; everything else is stringified for display.
 */
function SensitiveToolParams({ toolArgs }: SensitiveToolParamsProps): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const contentId = useId();

  const paramEntries = toolArgs !== null && typeof toolArgs === 'object' ? Object.entries(toolArgs) : [];
  if (paramEntries.length === 0) return null;

  return (
    <Box
      sx={{
        width: '100%',
        border: '1px solid',
        borderColor: 'divider',
        // eslint-disable-next-line elitea/ad-hoc-radius — nested params list border radius
        borderRadius: 1,
        overflow: 'hidden',
      }}
    >
      <Box
        onClick={() => setExpanded((prev) => !prev)}
        sx={{ display: 'flex', alignItems: 'center', px: 1, py: 0.5, cursor: 'pointer' }}
      >
        <Typography
          variant="caption"
          sx={{ fontWeight: 600, color: 'text.secondary' }}
        >
          {/* eslint-disable-next-line i18next/no-literal-string — collapsible section label */}
          Parameters {expanded ? '▾' : '▸'}
        </Typography>
      </Box>
      <Collapse
        in={expanded}
        id={contentId}
      >
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25, px: 1, py: 0.5 }}>
          {paramEntries.map(([key, value]) => (
            <Box
              key={key}
              sx={{ display: 'flex', gap: 1 }}
            >
              <Typography
                variant="caption"
                sx={{ fontWeight: 600, color: 'text.secondary', flexShrink: 0 }}
              >
                {key}:
              </Typography>
              <Typography
                variant="caption"
                sx={{ color: 'text.primary', wordBreak: 'break-word' }}
              >
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

interface SensitiveToolCardProps {
  readonly hitlInterrupt: HitlInterrupt;
  readonly disabled: boolean;
  readonly canBlockWithComment: boolean;
  readonly onApprove: () => void;
  readonly onReject: () => void;
  readonly onBlockWithComment: (comment: string) => void;
}

/** The sensitive-tool authorization-required card (`guardrail_type` branch). */
function SensitiveToolCard({
  hitlInterrupt,
  disabled,
  canBlockWithComment,
  onApprove,
  onReject,
  onBlockWithComment,
}: SensitiveToolCardProps): ReactNode {
  return (
    <Box
      data-testid="chat-hitl-actions"
      sx={{
        mt: 1,
        p: 1.5,
        border: '1px solid',
        borderColor: 'warning.main',
        // eslint-disable-next-line elitea/ad-hoc-radius — warning banner border radius
        borderRadius: 1,
        backgroundColor: 'warning.lighter',
        display: 'flex',
        flexDirection: 'column',
        gap: 0.75,
      }}
    >
      <Typography
        variant="subtitle2"
        sx={{ color: 'warning.dark', fontWeight: 600 }}
      >
        {/* eslint-disable-next-line i18next/no-literal-string — HITL authorization card title */}
        ⚠️ Sensitive Action Authorization Required
      </Typography>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
        <Typography
          variant="caption"
          sx={{ color: 'warning.dark' }}
        >
          {/* eslint-disable-next-line i18next/no-literal-string — HITL authorization card copy */}
          Agent is about to perform:
        </Typography>
        <Typography
          variant="body2"
          sx={{ fontWeight: 600 }}
        >
          {/* eslint-disable-next-line i18next/no-literal-string — fallback for a missing tool label */}
          {hitlInterrupt.action_label || hitlInterrupt.tool_name || 'Unknown action'}
        </Typography>
      </Box>
      {hitlInterrupt.tool_args !== undefined && hitlInterrupt.tool_args !== null && (
        <SensitiveToolParams toolArgs={hitlInterrupt.tool_args} />
      )}
      {hitlInterrupt.policy_message && (
        <Typography
          variant="caption"
          sx={{ color: 'warning.dark', fontStyle: 'italic' }}
        >
          {hitlInterrupt.policy_message}
        </Typography>
      )}
      <Stack
        direction="row"
        spacing={1}
        sx={{ flexWrap: 'wrap' }}
      >
        {canBlockWithComment ? (
          <BlockWithCommentControl
            onApprove={onApprove}
            onReject={onBlockWithComment}
            disabled={disabled}
          />
        ) : (
          <>
            <Button
              size="small"
              variant="contained"
              color="success"
              onClick={onApprove}
              disabled={disabled}
            >
              {/* eslint-disable-next-line i18next/no-literal-string — HITL action label */}
              Authorize
            </Button>
            <Button
              size="small"
              variant="outlined"
              color="error"
              onClick={onReject}
              disabled={disabled}
            >
              {/* eslint-disable-next-line i18next/no-literal-string — HITL action label */}
              Block
            </Button>
          </>
        )}
      </Stack>
    </Box>
  );
}

/**
 * `ChatHitlActions` — renders approval/edit/reject buttons for HITL
 * paused agent execution, or a dedicated authorization card for a
 * sensitive-tool pause.
 */
export function ChatHitlActions({
  hitlInterrupt,
  toolCallId,
  onHitlResume,
  disabled = false,
}: ChatHitlActionsProps): ReactNode {
  if (!hitlInterrupt || hitlInterrupt.decided) return null;

  const actions = hitlInterrupt.available_actions ?? ['approve', 'reject'];
  const canBlockWithComment = actions.includes('block_with_comment');
  const isSensitiveTool =
    hitlInterrupt.guardrail_type !== undefined && SENSITIVE_GUARDRAIL_TYPES.has(hitlInterrupt.guardrail_type);

  // The clarification pause. Its `available_actions` is `['answer']` alone, so
  // it matches none of the approve/reject/edit branches below and rendered a
  // card with no controls at all until this branch existed.
  const canAnswer = actions.includes(ANSWER_ACTION);
  const handleAnswer = (value: string): void => onHitlResume?.({ action: 'answer', value, toolCallId });
  const handleApprove = (): void => onHitlResume?.({ action: 'approve', toolCallId });
  const handleReject = (): void => onHitlResume?.({ action: 'reject', toolCallId });
  const handleEditSubmit = (value: string): void => onHitlResume?.({ action: 'edit', value, toolCallId });
  /**
   * #950 (ELITEA-1013): the comment box is labelled "Add a comment
   * (optional)...", and the route's contract is deliberate — `reject` refuses
   * a value, `block_with_comment` REQUIRES one
   * (`agentexecution/continue.go`'s `validCurrentHITLDecision`). Sending
   * `block_with_comment` with an empty comment was therefore refused with
   * `400 Invalid agent execution request` and the pause stayed open with
   * nothing said, on the one path a user takes without thinking. A decline
   * with no comment IS a plain reject, so the card sends the action its own
   * input actually describes. The comment itself travels unchanged (not
   * trimmed) whenever there is one.
   */
  const handleBlockWithComment = (comment: string): void => {
    if (comment.trim() === '' && actions.includes('reject')) {
      handleReject();
      return;
    }
    onHitlResume?.({ action: 'block_with_comment', value: comment, toolCallId });
  };

  if (canAnswer) {
    return (
      <ClarifyingQuestionCard
        questions={hitlInterrupt.questions ?? []}
        message={hitlInterrupt.message}
        disabled={disabled}
        onSubmit={handleAnswer}
      />
    );
  }

  if (isSensitiveTool) {
    return (
      <SensitiveToolCard
        hitlInterrupt={hitlInterrupt}
        disabled={disabled}
        canBlockWithComment={canBlockWithComment}
        onApprove={handleApprove}
        onReject={handleReject}
        onBlockWithComment={handleBlockWithComment}
      />
    );
  }

  return (
    <Box
      data-testid="chat-hitl-actions"
      sx={{
        mt: 1,
        p: 1.5,
        border: '1px solid',
        borderColor: 'warning.main',
        // eslint-disable-next-line elitea/ad-hoc-radius — warning banner border radius
        borderRadius: 1,
        backgroundColor: 'warning.lighter',
      }}
    >
      {hitlInterrupt.message && (
        <Typography
          variant="body2"
          sx={{ mb: 1, color: 'warning.dark' }}
        >
          {hitlInterrupt.message}
        </Typography>
      )}
      <Stack
        direction="row"
        spacing={1}
        sx={{ flexWrap: 'wrap' }}
      >
        {/* eslint-disable-next-line i18next/no-literal-string — action key comparison */}
        {!canBlockWithComment && actions.includes('approve') && (
          <Button
            size="small"
            variant="contained"
            color="success"
            onClick={handleApprove}
            disabled={disabled}
          >
            {/* eslint-disable-next-line i18next/no-literal-string — HITL action label */}
            Approve
          </Button>
        )}
        {/* eslint-disable-next-line i18next/no-literal-string — action key comparison */}
        {!canBlockWithComment && actions.includes('reject') && (
          <Button
            size="small"
            variant="outlined"
            color="error"
            onClick={handleReject}
            disabled={disabled}
          >
            {/* eslint-disable-next-line i18next/no-literal-string — HITL action label */}
            Reject
          </Button>
        )}
        {/* eslint-disable-next-line i18next/no-literal-string — action key comparison */}
        {actions.includes('edit') && (
          <EditControl
            currentValue=""
            onSubmit={handleEditSubmit}
            disabled={disabled}
          />
        )}
        {canBlockWithComment && (
          <BlockWithCommentControl
            onApprove={handleApprove}
            onReject={handleBlockWithComment}
            disabled={disabled}
          />
        )}
      </Stack>
    </Box>
  );
}
