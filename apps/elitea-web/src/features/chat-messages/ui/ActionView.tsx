/**
 * Ported from `apps/elitea-ui/src/components/Chat/ActionView.jsx` — renders
 * a single tool action in the streaming/complete view.
 *
 * Port of `apps/elitea-ui/src/components/Chat/ActionView.jsx`.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ToolModal } from './ToolModal';

/** @public Props for `ActionView`. */
export interface ActionViewProps {
  /** The action data to render. */
  readonly action: {
    readonly type?: string;
    readonly name?: string;
    readonly content?: string;
    readonly toolInputs?: unknown;
    readonly toolOutputs?: string;
    /**
     * A chunked tool output (#956) whose chunks did not all arrive, or whose
     * assembly did not match the digest its producer stamped. The row says so
     * rather than presenting a prefix as the result.
     */
    readonly toolOutputPartial?: boolean;
    readonly toolMeta?: Record<string, unknown>;
    readonly isError?: boolean;
    readonly status?: string;
    readonly timestamp?: string;
    /** A pin rebuilt from a persisted trace row — see `ToolModalProps` (#951). */
    readonly traceStepId?: number;
    readonly traceMessageGroupId?: number;
  };
  /** Called when the action is clicked. */
  readonly onClick?: (() => void) | undefined;
  /** Whether the action is selected. */
  readonly isSelected?: boolean;
}

/**
 * `ActionView` — renders a single tool action (thinking step, tool call,
 * or LLM response) in the streaming or completed state. Clicking it opens
 * a `ToolModal` with that action's full input/output detail.
 */
export function ActionView({ action, onClick, isSelected = false }: ActionViewProps): ReactNode {
  const [isModalOpen, setIsModalOpen] = useState(false);
  const displayText = action.content || action.toolOutputs || '';
  const isError = action.isError || action.status === 'error';

  const handleClick = (): void => {
    onClick?.();
    setIsModalOpen(true);
  };

  return (
    <>
      <Box
        // The only handle a browser test has on a tool INVOCATION row. The
        // row's text is the tool name, which also appears in the answer text
        // beside it, so `getByText` cannot tell "the runtime ran a tool" from
        // "the model mentioned one" (measured: the toolkit journeys' mock
        // quotes the tool result in the reply). `data-tool-action-name`
        // carries the name so the assertion names one row rather than
        // matching whichever row happens to contain the string.
        data-testid="chat-tool-action"
        data-tool-action-name={action.name ?? ''}
        onClick={handleClick}
        sx={{
          p: 1,
          mb: 0.5,
          borderRadius: 0.5,
          border: isSelected ? '1px solid' : '1px solid transparent',
          borderColor: isError ? 'error.main' : isSelected ? 'primary.main' : 'transparent',
          backgroundColor: isError ? 'error.lighter' : isSelected ? 'action.selected' : 'transparent',
          cursor: 'pointer',
          fontFamily: 'monospace',
          fontSize: '0.8rem',
          overflow: 'hidden',
        }}
      >
        <Typography
          variant="caption"
          sx={{
            display: 'block',
            color: isError ? 'error.main' : 'primary.main',
            fontWeight: 600,
            mb: 0.25,
          }}
        >
          {action.name || action.type || 'Action'}
        </Typography>
        <Typography
          variant="caption"
          sx={{
            // Clamped rather than free-flowing since a reasoning model's row
            // holds its ENTIRE chain of thought (`lib/chatStreamReasoning.ts`),
            // which is hundreds of lines and would bury the answer under its
            // own preview. Clamping, not scrolling: a scroll region inside a
            // click-to-open row swallows the page's own wheel events, and the
            // full text is one click away in the `ToolModal` this row opens.
            display: '-webkit-box',
            WebkitBoxOrient: 'vertical',
            WebkitLineClamp: 8,
            overflow: 'hidden',
            color: 'text.secondary',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >
          {displayText || '...'}
        </Typography>
        {action.toolOutputPartial ? (
          // Said on the ROW, not only in the modal: the preview above is a
          // prefix that ends mid-document and otherwise reads exactly like a
          // complete small result.
          <Typography
            data-testid="chat-tool-action-partial-output"
            variant="caption"
            sx={{ display: 'block', mt: 0.25, color: 'warning.main', fontWeight: 600 }}
          >
            {t('chatMessages.actionView.partialOutput', 'Partial output — some of this result did not arrive.')}
          </Typography>
        ) : null}
      </Box>
      <ToolModal
        open={isModalOpen}
        onClose={() => setIsModalOpen(false)}
        toolAction={action}
      />
    </>
  );
}
