/**
 * Sub-components for `./RunStateDialog.tsx`, split out purely to keep that
 * file under the §3.5 400-line budget (same split rationale as
 * `./RunStateDialog.styles.ts`). Ported from the private components at the
 * top of `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/state/
 * RunStateDialog.jsx` (baseline lines 30-151) plus `@/components/
 * PipelineStateViewModal.jsx` (`StateValueModal` below).
 *
 * `StateValueModal`: baseline `PipelineStateViewModal` is a top-level app
 * component (`@/components/PipelineStateViewModal.jsx`), not part of any
 * sub-unit's port scope and not built anywhere in this app — same class of
 * gap as `StyledInputModal`/`AlertDialog` (see `./StateVariableItem.tsx`/
 * `./StateVariableTable.tsx` doc comments). Unlike those two, the baseline
 * itself is a small, self-contained `Dialog`/`DialogTitle`/`DialogContent`
 * composition with no shared abstraction underneath — no in-scope
 * `shared/ui` primitive fits its fixed non-fullscreen sizing any better
 * than reproducing the same three MUI primitives directly, so it is ported
 * as a local, unexported component here rather than forced onto
 * `BaseModal`/`ExpandedViewerModal`.
 */
import type { ReactNode } from 'react';

import Close from '@mui/icons-material/Close';
import FullscreenOutlinedIcon from '@mui/icons-material/FullscreenOutlined';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import StepConnector from '@mui/material/StepConnector';
import Typography from '@mui/material/Typography';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import {
  iconInactiveSx,
  processConnectorSx,
  processStepIconInnerSx,
  processStepIconOuterSx,
  progressBoxSx,
  runStatusContainerSx,
  runStatusTextSx,
  stateItemViewContainerSx,
  stateItemViewHeaderContainerSx,
  stateItemViewHeaderIconButtonSx,
  stateItemViewSectionSx,
  stateItemViewValueBoxSx,
} from './RunStateDialog.styles';

export interface ProcessConnectorProps {
  readonly isError: boolean;
  /** The baseline's own trailing "fewer than 2 steps" connector override (`display: 'none'` when `false`). */
  readonly visible?: boolean;
}

export function ProcessConnector({ isError, visible = true }: ProcessConnectorProps): ReactNode {
  return <StepConnector sx={processConnectorSx(isError, visible)} />;
}

interface StateItemViewHeaderProps {
  readonly title: string;
  readonly onFullScreen: () => void;
}

function StateItemViewHeader({ title, onFullScreen }: StateItemViewHeaderProps): ReactNode {
  return (
    <Box sx={stateItemViewHeaderContainerSx}>
      <Typography
        variant="labelMedium"
        color="text.default"
      >
        {title}
      </Typography>
      <IconButton
        aria-label={t('pipelines.flowEditor.state.fullScreenView', 'Full screen view')}
        sx={stateItemViewHeaderIconButtonSx}
        onClick={onFullScreen}
      >
        <FullscreenOutlinedIcon fontSize="small" />
      </IconButton>
    </Box>
  );
}

export interface StateItemViewProps {
  readonly onFullScreen: (name: string, value: unknown) => void;
  readonly name: string;
  readonly valueBefore: unknown;
  readonly valueAfter: unknown;
  /** The last step of a finished run: the second value is the run's final state, and is labelled so (#6883). */
  readonly isFinal?: boolean;
}

export function StateItemView({ onFullScreen, name, valueBefore, valueAfter, isFinal = false }: StateItemViewProps): ReactNode {
  const afterTitle = isFinal
    ? t('pipelines.flowEditor.state.finalState', 'Final state')
    : t('pipelines.flowEditor.state.after', 'After');
  return (
    <Box sx={stateItemViewContainerSx}>
      <Box sx={stateItemViewSectionSx}>
        <StateItemViewHeader
          title={t('pipelines.flowEditor.state.before', 'Before')}
          onFullScreen={() => onFullScreen(name, valueBefore)}
        />
        <Box sx={stateItemViewValueBoxSx}>{JSON.stringify(valueBefore)}</Box>
      </Box>
      <Box sx={stateItemViewSectionSx}>
        <StateItemViewHeader
          title={afterTitle}
          onFullScreen={() => onFullScreen(name, valueAfter)}
        />
        <Box sx={stateItemViewValueBoxSx}>{JSON.stringify(valueAfter)}</Box>
      </Box>
    </Box>
  );
}

export interface ProcessStepIconProps {
  readonly active: boolean;
  readonly tooltip: string;
  readonly index: number;
  readonly onSelect: (index: number) => void;
  readonly isError: boolean;
}

export function ProcessStepIcon({ active, tooltip, index, onSelect, isError }: ProcessStepIconProps): ReactNode {
  const select = (): void => onSelect(index);

  return (
    <Tooltip
      title={tooltip}
      placement="top"
    >
      {/* Baseline: a plain `<Box onClick>` with no keyboard affordance at
          all (a `<div>`, not a real button). This app's `jsx-a11y` gates
          are stricter than the baseline ever enforced — a real native
          `<button>` (via `Box component="button"`) both satisfies
          `prefer-tag-over-role` (no `role="button"` on a non-button
          element) and gets Enter/Space activation for free, so no manual
          `onKeyDown` is needed either. Not a baseline behaviour, a real
          a11y fix, same class of addition `shared/ui/BasicAccordion.tsx`'s
          own doc comment documents for its `aria-labelledby` gap. */}
      <Box
        component="button"
        type="button"
        aria-label={tooltip}
        sx={processStepIconOuterSx(active, isError)}
        onClick={select}
      >
        <Box sx={processStepIconInnerSx(isError)} />
      </Box>
    </Tooltip>
  );
}

export interface RunStatusProps {
  readonly status: string;
}

export function RunStatus({ status }: RunStatusProps): ReactNode {
  return (
    <Box sx={runStatusContainerSx(status)}>
      <Typography
        component="div"
        variant="labelSmall"
        sx={runStatusTextSx(status)}
      >
        {status}
      </Typography>
    </Box>
  );
}

export interface InProgressIndicatorProps {
  readonly stepName: string;
}

/** The timeline header's live "Performing..." indicator — split out of `./RunStateDialog.tsx` for the same complexity-budget reason as its siblings above. */
export function InProgressIndicator({ stepName }: InProgressIndicatorProps): ReactNode {
  return (
    <>
      <Typography
        variant="bodyMedium"
        color="text.secondary"
      >
        {`${stepName}:`}
      </Typography>
      <Box sx={progressBoxSx}>
        <CircularProgress
          size={14}
          sx={iconInactiveSx}
        />
      </Box>
      <Typography
        variant="bodyMedium"
        sx={iconInactiveSx}
      >
        {t('pipelines.flowEditor.state.performing', 'Performing')}
      </Typography>
    </>
  );
}

export interface StateValueModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly label: string | undefined;
  readonly value: unknown;
}

export function StateValueModal({ open, onClose, label, value }: StateValueModalProps): ReactNode {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      slotProps={{ paper: { sx: stateValueModalPaperSx } }}
    >
      <DialogTitle
        variant="headingMedium"
        color="text.secondary"
        sx={stateValueModalTitleSx}
      >
        <Box sx={stateValueModalTitleRowSx}>
          {label}
          <IconButton
            aria-label={t('pipelines.flowEditor.state.close', 'Close')}
            sx={stateItemViewHeaderIconButtonSx}
            onClick={onClose}
          >
            <Close fontSize="small" />
          </IconButton>
        </Box>
      </DialogTitle>
      <DialogContent sx={stateValueModalContentSx}>{JSON.stringify(value)}</DialogContent>
    </Dialog>
  );
}

const stateValueModalPaperSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
  borderRadius: theme.vars.shape.radiusLg,
  border: `.0625rem solid ${theme.vars.palette.border.lines}`,
  boxShadow: theme.vars.palette.boxShadow.default,
  margin: 0,
  maxWidth: '90vw',
  height: 'calc(100vh - 10rem)',
});

const stateValueModalTitleSx: SxProps<Theme> = (theme: Theme) => ({
  height: '3.75rem',
  padding: `${theme.spacing(2)} ${theme.spacing(4)}`,
});

const stateValueModalTitleRowSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'row',
  justifyContent: 'space-between',
  alignItems: 'center',
};

const stateValueModalContentSx: SxProps<Theme> = (theme: Theme) => ({
  padding: `${theme.spacing(2)} ${theme.spacing(4)}`,
  width: '80vw',
  maxWidth: '56.25rem',
  height: 'calc(100vh - 13.75rem)',
  borderTop: `.0625rem solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.showContextDialog,
  overflowY: 'auto',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
});
