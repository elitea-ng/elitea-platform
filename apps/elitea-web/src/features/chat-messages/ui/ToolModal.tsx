/**
 * Ported from `apps/elitea-ui/src/components/Chat/ToolModal.jsx` — renders
 * a modal for viewing tool execution details.
 *
 * The first pass rendered three stacked `<pre>` blocks in a `maxWidth="md"`
 * dialog. This pass restores the baseline's real layout: a wide dialog
 * whose body is a 50/50 horizontal split, `INPUT` on the left and `OUTPUT`
 * on the right, each a read-only CodeMirror editor with a line-number
 * gutter, a language selector and a copy button (see `ToolModalPane`).
 *
 * Deviations from the baseline:
 *  - The split is a CSS grid, not `react-split`: the draggable gutter would
 *    need a new runtime dependency (a toolchain-level decision, spec §2.5)
 *    for a resize affordance, while the 50/50 layout itself is the part the
 *    user actually sees. Resizing is a disclosed, deliberate gap.
 *  - `toolAction.content` has no pane of its own (the baseline modal takes
 *    `input`/`output` only). It is used as the OUTPUT text when the action
 *    carries no `toolOutputs`, which is how `ActionView` already treats the
 *    two fields for its own preview line.
 */
import type { ReactNode } from 'react';

import Dialog from '@mui/material/Dialog';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import CloseIcon from '@mui/icons-material/Close';

import { t } from '@/shared/i18n';
import { toolPayloadText } from '@/shared/lib/toolPayloadText';

import { useTraceStepDetail } from '../model/traceStepDetail';

import { ToolModalPane } from './ToolModalPane';

/** @public Props for `ToolModal`. */
export interface ToolModalProps {
  /** Whether the modal is open. */
  readonly open: boolean;
  /** Called when the modal is closed. */
  readonly onClose: () => void;
  /** The tool action data to display. */
  readonly toolAction: {
    readonly name?: string;
    readonly type?: string;
    readonly toolInputs?: unknown;
    readonly toolOutputs?: unknown;
    readonly toolMeta?: Record<string, unknown>;
    readonly content?: string;
    readonly isError?: boolean;
    /**
     * Set only on a pin rebuilt from a persisted `chat_message_trace_step`
     * row (#951). The listing that rebuilt it is deliberately light, so the
     * body below is fetched for this one row when the modal opens.
     */
    readonly traceStepId?: number;
    readonly traceMessageGroupId?: number;
  };
}

/**
 * `"<type> - <name>"` when the action carries both (baseline
 * `ToolModal.jsx:61`'s two-part title), otherwise whichever half exists.
 */
function toolModalTitle(type: string | undefined, name: string | undefined): string {
  if (type && name) return `${type} - ${name}`;
  return name || type || t('chatMessages.toolModal.defaultTitle', 'Tool Details');
}

const styles = {
  paper: (theme: Theme) => ({
    backgroundColor: theme.vars.palette.background.secondary,
    border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    borderRadius: theme.vars.shape.radiusSm,
    maxWidth: '75rem',
    minHeight: '80vh',
    maxHeight: '90vh',
  }),
  title: (theme: Theme) => ({
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    gap: theme.spacing(3),
    padding: theme.spacing(2, 4),
    backgroundColor: theme.vars.palette.background.tabPanel,
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  }),
  content: (theme: Theme) => ({
    padding: 0,
    display: 'grid',
    gridTemplateColumns: { xs: '1fr', md: '1fr 1fr' },
    columnGap: '0.0625rem',
    backgroundColor: theme.vars.palette.border.lines,
    overflow: 'hidden',
    minHeight: 0,
    flex: 1,
  }),
} as const;

/**
 * `ToolModal` — a wide detail modal for one tool execution, split
 * `INPUT` | `OUTPUT` across two read-only code editors.
 */
export function ToolModal({ open, onClose, toolAction }: ToolModalProps): ReactNode {
  const ownInput = toolPayloadText(toolAction.toolInputs);
  const ownOutput = toolPayloadText(toolAction.toolOutputs ?? toolAction.content);
  // Only a RESTORED pin with nothing of its own asks for anything: a live step
  // carries its body and no row identity, so this never fires for one.
  const detail = useTraceStepDetail(toolAction, open && ownInput === '' && ownOutput === '');
  const inputText = ownInput === '' && detail !== undefined ? toolPayloadText(detail.toolInputs) : ownInput;
  const outputText = ownOutput === '' && detail !== undefined ? detail.output : ownOutput;

  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="xl"
      fullWidth
      slotProps={{ paper: { sx: styles.paper } }}
    >
      <DialogTitle sx={styles.title}>
        <Typography variant="h6">{toolModalTitle(toolAction.type, toolAction.name)}</Typography>
        <IconButton
          size="small"
          aria-label={t('common.closeAriaLabel', 'Close')}
          onClick={onClose}
        >
          <CloseIcon />
        </IconButton>
      </DialogTitle>
      <DialogContent sx={styles.content}>
        <ToolModalPane
          caption={t('chatMessages.toolModal.inputCaption', 'INPUT')}
          value={inputText}
          paneId="input"
        />
        <ToolModalPane
          caption={t('chatMessages.toolModal.outputCaption', 'OUTPUT')}
          value={outputText}
          paneId="output"
        />
      </DialogContent>
    </Dialog>
  );
}
