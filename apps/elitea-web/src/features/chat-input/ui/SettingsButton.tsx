import type { ReactNode } from 'react';

import Button from '@mui/material/Button';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { GearIcon } from '@/shared/ui/icons/gear-icon';

import { t } from '@/shared/i18n';

import type { AgentEditorPanelStyles } from './AgentEditorPanel.styles';

/** The agent/pipeline-settings button — factored out of `AgentEditorPanel.tsx` for the same §3.5 reason as its sibling sub-components. */
export function SettingsButton({
  tooltip,
  onClick,
  disabled,
  isBeingEdited,
  canEdit,
  styles,
}: {
  readonly tooltip: string;
  readonly onClick: (() => void) | undefined;
  readonly disabled: boolean;
  readonly isBeingEdited: boolean;
  readonly canEdit: boolean;
  readonly styles: AgentEditorPanelStyles;
}): ReactNode {
  const state = canEdit ? t('chatInput.agentEditorPanel.editing', 'Editing…') : t('chatInput.agentEditorPanel.viewing', 'Viewing…');
  // The label carries the visible "Editing…" text too (WCAG 2.5.3).
  const label = isBeingEdited
    ? t('chatInput.agentEditorPanel.settingsMenuLabelState', 'agent settings menu: {{state}}', { state })
    : t('chatInput.agentEditorPanel.settingsMenuLabel', 'agent settings menu');
  return (
    <Tooltip
      placement="top"
      title={tooltip}
    >
      <Button
        size="small"
        aria-label={label}
        aria-haspopup="dialog"
        onClick={onClick}
        disabled={disabled}
        variant="elitea"
        color="secondary"
      >
        {isBeingEdited ? (
          <Typography
            variant="labelSmall"
            color="primary"
            sx={styles.editingText}
          >
            {state}
          </Typography>
        ) : (
          <GearIcon style={gearIconStyle} />
        )}
      </Button>
    </Tooltip>
  );
}

const gearIconStyle = { width: '1rem', height: '1rem' };
