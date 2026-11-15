import type { MouseEvent, ReactNode } from 'react';

import PushPinIcon from '@mui/icons-material/PushPin';
import PushPinOutlinedIcon from '@mui/icons-material/PushPinOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

interface PinToggleProps {
  readonly entityName: string;
  readonly pinned: boolean;
  readonly onToggle: (pinned: boolean) => void;
}

/** Pin or unpin one row to the caller's own Pinned group. Private: another member never sees it. */
export function PinToggle({ entityName, pinned, onToggle }: PinToggleProps): ReactNode {
  const label = pinned
    ? t('widgets.entityFolders.unpin', 'Unpin {{name}}', { name: entityName })
    : t('widgets.entityFolders.pin', 'Pin {{name}}', { name: entityName });
  return (
    <Tooltip title={pinned ? t('widgets.entityFolders.unpinShort', 'Unpin') : t('widgets.entityFolders.pinShort', 'Pin')}>
      <IconButton
        size="small"
        aria-label={label}
        aria-pressed={pinned}
        data-testid="entity-pin-toggle"
        onClick={(event: MouseEvent<HTMLElement>) => {
          event.stopPropagation();
          onToggle(!pinned);
        }}
      >
        {pinned ? <PushPinIcon fontSize="small" /> : <PushPinOutlinedIcon fontSize="small" />}
      </IconButton>
    </Tooltip>
  );
}
