/**
 * `HeaderActions` and `StatusIndicatorRow` for `./RunStateDialog.tsx`,
 * split into their own file purely to keep `RunStateDialog` itself under
 * the §3.5 `complexity` budget (12) — the header's Stop/Delete ternary and
 * the timeline header's 5-branch last-step status display were, between
 * them, most of that component's reported complexity of 26. Ported from
 * `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/state/
 * RunStateDialog.jsx`'s own JSX (baseline lines 214-253 and 273-367) — no
 * behaviour change, only relocated into named sub-components: the
 * baseline renders its `{stepId}:` label once, outside/before the
 * 5-branch status dispatch, so every branch component below
 * (`ErrorBranch`/`InterruptBranch`/`CompletedBranch`/`StoppedBranch`, plus
 * `InProgressIndicator` in `./RunStateDialog.parts.tsx`) independently
 * renders its own `<StepLabel>` to reproduce that same always-rendered
 * label at each of the 5 call sites.
 */
import type { ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
// `ErrorOutline` (bare, no style suffix) does not exist in this app's pinned
// `@mui/icons-material@9.2.0` (verified: no `ErrorOutline.d.ts` in
// `node_modules/@mui/icons-material/`, only `ErrorOutline{Outlined,Rounded,
// Sharp,TwoTone}` and the unrelated `Error`/`ErrorOutlined` glyphs).
// `ErrorOutlineOutlined` is the same glyph this app's other "*Outlined"
// icons already establish as the default style (`DeleteOutlined`,
// `FullscreenOutlined`).
import ErrorOutlineIcon from '@mui/icons-material/ErrorOutlineOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { CollapseIcon } from '@/shared/ui/icons/collapse-icon';
import { StopIcon } from '@/shared/ui/icons/stop-icon';
import { t } from '@/shared/i18n';

import { InProgressIndicator } from './RunStateDialog.parts';
import {
  attentionIconSx,
  headerIconButtonSx,
  iconErrorSx,
  iconInactiveSx,
  statusIndicatorSx,
  textErrorSx,
  textPublishedSx,
} from './RunStateDialog.styles';

export interface HeaderActionsProps {
  readonly status: string;
  readonly recoveryPaused?: boolean;
  readonly onStop: (event: { readonly stopPropagation: () => void }) => void;
  readonly onDelete: (event: { readonly stopPropagation: () => void }) => void;
  readonly onClose: () => void;
}

export function HeaderActions({ status, recoveryPaused = false, onStop, onDelete, onClose }: HeaderActionsProps): ReactNode {
  return (
    <>
      {(status === FlowEditorConstants.PipelineStatus.InProgress || (recoveryPaused && status === FlowEditorConstants.PipelineStatus.Interrupt)) ? (
        <IconButton
          aria-label={t('pipelines.flowEditor.state.stopRun', 'Stop run')}
          sx={headerIconButtonSx}
          onClick={onStop}
        >
          <StopIcon
            width="16"
            height="16"
          />
        </IconButton>
      ) : (
        <IconButton
          aria-label={t('pipelines.flowEditor.state.deleteRun', 'Delete run')}
          sx={headerIconButtonSx}
          onClick={onDelete}
        >
          <DeleteOutlineIcon fontSize="small" />
        </IconButton>
      )}
      <IconButton
        aria-label={t('pipelines.flowEditor.state.close', 'Close')}
        sx={headerIconButtonSx}
        onClick={onClose}
      >
        <CollapseIcon
          width="16"
          height="16"
        />
      </IconButton>
    </>
  );
}

/** Not `@public` — only `StatusIndicatorRowProps.lastStep` (below) references it; a bare `export` here would be dead public surface (no external caller needs the shape on its own). */
interface StatusIndicatorRowStep {
  readonly id?: string;
  readonly status?: string;
}

export interface StatusIndicatorRowProps {
  readonly visible: boolean;
  readonly lastStep: StatusIndicatorRowStep | undefined;
}

function StepLabel({ stepId }: { readonly stepId: string }): ReactNode {
  return (
    <Typography
      variant="bodyMedium"
      color="text.secondary"
    >
      {`${stepId}:`}
    </Typography>
  );
}

function ErrorBranch({ stepId }: { readonly stepId: string }): ReactNode {
  return (
    <>
      <StepLabel stepId={stepId} />
      <ErrorOutlineIcon sx={iconErrorSx} />
      <Typography
        variant="bodyMedium"
        sx={textErrorSx}
      >
        {t('pipelines.flowEditor.state.error', 'Error')}
      </Typography>
    </>
  );
}

function InterruptBranch({ stepId }: { readonly stepId: string }): ReactNode {
  return (
    <>
      <StepLabel stepId={stepId} />
      <Box sx={attentionIconSx}>
        <AttentionIcon
          width="14"
          height="14"
        />
      </Box>
      <Typography
        variant="bodyMedium"
        sx={iconInactiveSx}
      >
        {t('pipelines.flowEditor.state.userActionWaiting', 'User action waiting...')}
      </Typography>
    </>
  );
}

function CompletedBranch({ stepId }: { readonly stepId: string }): ReactNode {
  return (
    <>
      <StepLabel stepId={stepId} />
      <Typography
        variant="bodyMedium"
        sx={textPublishedSx}
      >
        {t('pipelines.flowEditor.state.completed', 'Completed')}
      </Typography>
    </>
  );
}

function StoppedBranch({ stepId }: { readonly stepId: string }): ReactNode {
  return (
    <>
      <StepLabel stepId={stepId} />
      <Box sx={attentionIconSx}>
        <AttentionIcon
          width="14"
          height="14"
        />
      </Box>
      <Typography
        variant="bodyMedium"
        sx={iconInactiveSx}
      >
        {t('pipelines.flowEditor.state.stopped', 'Stopped')}
      </Typography>
    </>
  );
}

/** One of five mutually-exclusive branches keyed off `lastStep.status`, dispatched via `switch` (not a chain of independent `&&` checks) purely to keep `StatusIndicatorRow` itself under the §3.5 `complexity` budget. */
function statusBranchContent(status: string, stepId: string): ReactNode {
  switch (status) {
    case FlowEditorConstants.PipelineStatus.InProgress:
      return <InProgressIndicator stepName={stepId} />;
    case FlowEditorConstants.PipelineStatus.Error:
      return <ErrorBranch stepId={stepId} />;
    case FlowEditorConstants.PipelineStatus.Interrupt:
      return <InterruptBranch stepId={stepId} />;
    case FlowEditorConstants.PipelineStatus.Completed:
      return <CompletedBranch stepId={stepId} />;
    case FlowEditorConstants.PipelineStatus.Stopped:
      return <StoppedBranch stepId={stepId} />;
    default:
      return null;
  }
}

/** The timeline header's last-step status readout. */
export function StatusIndicatorRow({ visible, lastStep }: StatusIndicatorRowProps): ReactNode {
  if (!visible) return null;

  return <Box sx={statusIndicatorSx}>{statusBranchContent(lastStep?.status ?? '', lastStep?.id ?? '')}</Box>;
}
