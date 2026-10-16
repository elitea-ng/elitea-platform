/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/RunStateNode.jsx` (197 lines) — unit A2f. Unlike this slice's other
 * `ui/nodes/*` files, `RunStateNode` is NOT a React-Flow-registered node
 * type — it is a small "run history" pill rendered by the flow editor's own
 * run-controls chrome, taking plain props (`deleteRunNode`/`onStopRun`/
 * `yamlJsonObject`/etc.) rather than React Flow's `{id, data, selected}`
 * contract. `./RunStateNodeGroup.tsx` is its only caller within this
 * sub-unit.
 *
 * CROSS-SUB-UNIT DEPENDENCY NOT YET LANDED: `FlowEditorState.RunStateDialog`
 * (baseline: `flow-editor/ui/state/RunStateDialog.jsx`) — explicitly called
 * out in this mission's own preamble as unit A2j's ownership, not yet built
 * anywhere in this worktree (verified: no `state/` directory exists under
 * `src/features/pipelines/ui/`). Imported below from the expected
 * `../../state/RunStateDialog` path (mirroring this batch's established
 * `ui/{select,settings,state}/<Component>.tsx` per-file convention); `tsc`
 * will report a real "cannot find module" error here until A2j lands.
 *
 * `Tooltip` (baseline: `@/ComponentsLib/Tooltip`) -> MUI `Tooltip`, same
 * substitution as `./DecisionNode/DecisionNodeShared.tsx`. `DeleteIcon`
 * (baseline: `@/components/Icons/DeleteIcon`, a custom SVG) -> MUI's
 * `DeleteOutlined`, the SAME already-established interim substitute
 * `ui/nodes/BaseNode/NodeCardHeader.tsx`'s own doc comment documents for
 * this exact gap (no `delete-icon.tsx` in S2's ported icon set).
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useState } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ErrorIcon } from '@/shared/ui/icons/error-icon';
import { FailIcon } from '@/shared/ui/icons/fail-icon';
import { StopIcon } from '@/shared/ui/icons/stop-icon';
import { SuccessIcon } from '@/shared/ui/icons/success-icon';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { RunStateDialog } from '../state/RunStateDialog';

export interface RunStateNodeData {
  readonly status: string;
  readonly label: string;
  readonly [key: string]: unknown;
}

export interface RunStateNodeProps {
  readonly avoidTooltip?: boolean | undefined;
  readonly data: RunStateNodeData;
  readonly id: string;
  readonly deleteRunNode: (id: string) => void;
  readonly onStopRun: (id: string) => void;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly editorHeight?: number | undefined;
  readonly editorWidth?: number | undefined;
  readonly selected?: boolean | undefined;
}

export const RunStateNode = memo(function RunStateNode(props: RunStateNodeProps): ReactNode {
  const { avoidTooltip, data, id, deleteRunNode, onStopRun, yamlJsonObject, editorHeight, editorWidth, selected } = props;

  const styles = runNodeStyles(data.status, Boolean(selected));

  const [isOpened, setIsOpened] = useState(false);
  const runInProgress = data.status === FlowEditorConstants.PipelineStatus.InProgress;
  const canStop = runInProgress || (data['recoveryPaused'] === true && data.status === FlowEditorConstants.PipelineStatus.Interrupt);

  const onOpen = useCallback(() => setIsOpened(true), []);
  const onClose = useCallback(() => setIsOpened(false), []);

  const onStop = useCallback(
    (event: { readonly stopPropagation: () => void }) => {
      event.stopPropagation();
      onStopRun(id);
    },
    [onStopRun, id],
  );

  const onDelete = useCallback(
    (event: { readonly stopPropagation: () => void }) => {
      event.stopPropagation();
      deleteRunNode(id);
      setIsOpened(false);
    },
    [id, deleteRunNode],
  );

  return (
    <>
      <Box sx={styles.wrapper}>
        <Tooltip
          title={avoidTooltip ? '' : t('pipelines.flowEditor.runStateNode.runIsStatus', 'Run is {{status}}', { status: data.status.toLowerCase() })}
          placement="bottom"
        >
          <Box sx={styles.statusIcon}>
            {(() => {
              switch (data.status) {
                case FlowEditorConstants.PipelineStatus.Completed:
                  return <SuccessIcon />;
                case FlowEditorConstants.PipelineStatus.Error:
                  return <ErrorIcon />;
                case FlowEditorConstants.PipelineStatus.InProgress:
                  return (
                    <CircularProgress
                      size={14}
                      thickness={5}
                    />
                  );
                default:
                  return <FailIcon />;
              }
            })()}
          </Box>
        </Tooltip>

        <Tooltip
          title={avoidTooltip ? '' : t('pipelines.flowEditor.runStateNode.viewDetails', 'View details')}
          placement="bottom"
        >
          <Typography
            variant="labelMedium"
            sx={styles.runName}
            onClick={onOpen}
          >
            {data.label}
          </Typography>
        </Tooltip>

        <Tooltip
          title={
            avoidTooltip
              ? ''
              : canStop
                ? t('pipelines.flowEditor.runStateNode.stopRun', 'Stop run')
                : t('pipelines.flowEditor.runStateNode.deleteRun', 'Delete run')
          }
          placement="bottom"
        >
          <Box sx={styles.negativeButton}>
            {canStop ? <StopIcon onClick={onStop} /> : <DeleteOutlineIcon onClick={onDelete} />}
          </Box>
        </Tooltip>
      </Box>
      <RunStateDialog
        data={data}
        state={yamlJsonObject.state}
        open={isOpened}
        onClose={onClose}
        onStop={onStop}
        onDelete={onDelete}
        editorHeight={editorHeight}
        editorWidth={editorWidth}
      />
    </>
  );
});

interface RunNodeStyles {
  readonly wrapper: SxProps<Theme>;
  readonly runName: SxProps<Theme>;
  readonly statusIcon: SxProps<Theme>;
  readonly negativeButton: SxProps<Theme>;
}

/**
 * `negativeButton.svg`'s in-progress fill and non-in-progress `:hover` fill
 * (baseline: `runNodeStyles`, ported here). The baseline's `:hover` branch
 * used `!important` to win against MUI icon defaults — dropped (R-T5, no
 * per-file waiver available); the sx-generated class already outranks the
 * base icon rule at equal specificity (same reasoning
 * `./DecisionNode/DecisionNodeShared.tsx`'s doc comment gives for its own
 * dropped `!important`s).
 * Nested `sx` selector values (unlike the top-level `sx` prop itself) are
 * plain `CSSObject`-shaped, not `SxProps<Theme>` — so this returns the
 * resolved plain object for a given `theme`, not a further theme-callback,
 * and is called inline from `runNodeStyles`'s own theme callback below.
 * `width`/`height` (not `fontSize`, R-T11 bans ad-hoc `fontSize:` literals)
 * size the icon to the baseline's same `1rem` square.
 */
function negativeButtonSvgStyle(status: string, theme: Theme): Record<string, unknown> {
  if (status === FlowEditorConstants.PipelineStatus.InProgress) {
    return { width: '1rem', height: '1rem', path: { fill: theme.vars.palette.status.onModeration } };
  }
  return {
    width: '1rem',
    height: '1rem',
    '&:hover': { path: { fill: theme.vars.palette.text.secondary } },
  };
}

function runNodeStyles(status: string, selected: boolean): RunNodeStyles {
  const iconColor = (theme: Theme): string => {
    switch (status) {
      case FlowEditorConstants.PipelineStatus.Completed:
        return theme.vars.palette.status.published;
      case FlowEditorConstants.PipelineStatus.Error:
        return theme.vars.palette.status.rejected;
      case FlowEditorConstants.PipelineStatus.Stopped:
        return theme.vars.palette.status.onModeration;
      default:
        return theme.vars.palette.icon.fill.inactive;
    }
  };

  return {
    wrapper: (theme: Theme) => ({
      padding: theme.spacing(0.375, 0.75),
      borderRadius: theme.vars.shape.radiusMd,
      border: `.0625rem solid ${selected ? theme.vars.palette.background.button.primary.disabled : theme.vars.palette.border.lines}`,
      background: selected ? theme.vars.palette.background.dataGrid.main : theme.vars.palette.background.tabPanel,
      height: theme.spacing(2.25),
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      gap: theme.spacing(0.5),
      '&:hover': {
        cursor: 'pointer',
        border: `.0625rem solid ${theme.vars.palette.background.button.primary.disabled}`,
        background: theme.vars.palette.background.dataGrid.main,
      },
    }),
    runName: (theme: Theme) => ({ color: theme.vars.palette.text.secondary }),
    statusIcon: (theme: Theme) => ({
      display: 'flex',
      justifyContent: 'flex-start',
      alignItems: 'center',
      cursor: 'pointer',
      path: { fill: iconColor(theme) },
      circle: { color: iconColor(theme) },
    }),
    negativeButton: (theme: Theme) => ({
      display: 'flex',
      justifyContent: 'center',
      alignItems: 'center',
      cursor: 'pointer',
      svg: negativeButtonSvgStyle(status, theme),
    }),
  };
}
