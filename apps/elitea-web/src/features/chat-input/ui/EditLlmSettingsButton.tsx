import type { ReactNode } from 'react';

import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import TuneIcon from '@mui/icons-material/Tune';

import { t } from '@/shared/i18n';

import { showEditLlmSettingsButton } from './AgentEditorPanel.derive';

/**
 * A14 (ELITEA-0386): per-participant "Edit LLM settings" trigger — factored
 * out of `AgentEditorPanel.tsx` for the same §3.5 cyclomatic-complexity
 * reason as its siblings (`SettingsButton`/`SwitchToModelButton`). Renders
 * nothing when the composition root supplies no `onEditLlmSettings` (same
 * "omit to withhold" convention `onSwitchToModel` already uses) or the
 * caller cannot edit this participant.
 */
export function EditLlmSettingsButton({
  onEditLlmSettings,
  canEdit,
  disabled,
}: {
  readonly onEditLlmSettings: (() => void) | undefined;
  readonly canEdit: boolean;
  readonly disabled: boolean;
}): ReactNode {
  if (!showEditLlmSettingsButton(onEditLlmSettings, canEdit)) return null;
  const label = t('chatInput.agentEditorPanel.editLlmSettingsTooltip', 'Edit LLM settings');
  return (
    <>
      <Divider orientation="vertical" />
      <Tooltip
        placement="top"
        title={label}
      >
        <IconButton
          size="small"
          aria-label={label}
          data-testid="chat-agent-editor-llm-settings-button"
          onClick={onEditLlmSettings}
          disabled={disabled}
        >
          <TuneIcon fontSize="small" />
        </IconButton>
      </Tooltip>
    </>
  );
}
