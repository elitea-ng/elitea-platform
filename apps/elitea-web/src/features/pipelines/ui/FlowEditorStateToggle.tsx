/**
 * `FlowEditor.jsx:500-522`'s "State" toggle button (shown whenever the
 * state drawer is closed), split into its own component purely to keep
 * `FlowEditor.tsx` itself under the §3.5 400-line file-length budget.
 *
 * `data-tour={PIPELINE_TOUR_TARGET_IDS.state}` is DROPPED — see
 * `FlowEditor.tsx`'s own doc comment for the full `interactive-tours`
 * out-of-scope disclosure.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { ClipboardIcon } from '@/shared/ui/icons/clipboard-icon';

import { flowEditorIconScaleSx, flowEditorStateDrawerButtonSx } from './FlowEditor.styles';

export interface FlowEditorStateToggleProps {
  readonly isOpen: boolean;
  readonly onToggle: () => void;
  readonly rightOffset?: number | undefined;
}

export function FlowEditorStateToggle({ isOpen, onToggle, rightOffset = 0 }: FlowEditorStateToggleProps): ReactNode {
  const theme = useTheme();

  if (isOpen) return null;

  return (
    <Box
      data-testid="pipeline-state-toggle-wrapper"
      data-right-offset={String(rightOffset)}
      sx={{
        position: 'absolute',
        top: theme.spacing(2.5),
        right: rightOffset > 0 ? `${(rightOffset + 20) / 16}rem` : theme.spacing(2.5),
        zIndex: 100,
        transition: 'right 0.2s ease',
      }}
    >
      <BaseBtn
        variant="elitea"
        color="secondary"
        onClick={onToggle}
        startIcon={
          <Box sx={flowEditorIconScaleSx}>
            <ClipboardIcon />
          </Box>
        }
        sx={flowEditorStateDrawerButtonSx}
      >
        {t('pipelines.flowEditor.stateButton', 'State')}
      </BaseBtn>
    </Box>
  );
}
