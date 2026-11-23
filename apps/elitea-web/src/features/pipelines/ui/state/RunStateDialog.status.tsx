/**
 * `HeaderActions` for `./RunStateDialog.tsx` (Stop / Delete / Close controls).
 * Timeline last-step status display moved to `ui/run-state-timeline/TimeLineRunStatus.tsx` (EL-3734).
 */
import type { ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import IconButton from '@mui/material/IconButton';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { CollapseIcon } from '@/shared/ui/icons/collapse-icon';
import { StopIcon } from '@/shared/ui/icons/stop-icon';
import { t } from '@/shared/i18n';

import { headerIconButtonSx } from './RunStateDialog.styles';

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
