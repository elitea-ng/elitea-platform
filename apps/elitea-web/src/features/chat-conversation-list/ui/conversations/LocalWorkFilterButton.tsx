import type { ReactNode } from 'react';

import LaptopMacOutlinedIcon from '@mui/icons-material/LaptopMacOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

/**
 * The Chats rail's "Local work" filter. Desktop Local work threads (an agent
 * ran each turn on someone's computer over a local folder) are left out of
 * the ordinary rail; this toggle lists them instead. A toggle, not a menu:
 * the rail has exactly two listings, and `aria-pressed` says which one is on.
 * Same 28px icon-button skin as `ConversationSearchButton` beside it.
 */
export interface LocalWorkFilterButtonProps {
  readonly on: boolean;
  readonly onChange: (on: boolean) => void;
}

/** Renders nothing without a filter to drive (`localWork` absent). */
export function LocalWorkFilterButton({ localWork }: { readonly localWork: LocalWorkFilterButtonProps | undefined }): ReactNode {
  return localWork === undefined ? null : <LocalWorkToggle {...localWork} />;
}

function LocalWorkToggle({ on, onChange }: LocalWorkFilterButtonProps): ReactNode {
  const label = on
    ? t('features.chatConversationList.localWork.hide', 'Back to chats')
    : t('features.chatConversationList.localWork.show', 'Show Local work threads from the desktop app');

  return (
    <Tooltip title={label} placement="top">
      <IconButton
        onClick={() => onChange(!on)}
        color="secondary"
        aria-label={t('features.chatConversationList.localWork.filter', 'Local work')}
        aria-pressed={on}
        data-testid="conversation-local-work-filter"
        sx={(theme) => ({
          minWidth: '28px',
          width: '28px',
          height: '28px',
          boxSizing: 'border-box',
          padding: theme.spacing(0.75),
          marginLeft: 0,
          backgroundColor: on ? theme.vars.palette.background.button.secondary.pressed : undefined,
        })}
      >
        <LaptopMacOutlinedIcon sx={{ width: '16px', height: '16px', color: (theme) => (on ? theme.vars.palette.icon.fill.active : theme.vars.palette.icon.fill.secondary) }} />
      </IconButton>
    </Tooltip>
  );
}
