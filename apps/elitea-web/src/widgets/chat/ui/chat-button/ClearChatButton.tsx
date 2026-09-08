import { memo } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

const clearChatLabel = t('widgets.chat.chatButton.clearChat.label', 'Clear chat');

/**
 * Chat button primitive: ClearChatButton
 *
 * Renders a trash / delete icon button that clears the current chat.
 *
 * Prop contract (injected by the composition root through `slots.renderClearChatButton`):
 *   - `disabled` — disable the button (e.g. when chat is empty)
 *   - `onClear`  — fire the clear action
 *   - `label`    — overrides the tooltip and the accessible name. The chat
 *     surface names the action after what it destroys ("Clear the chat
 *     history"), while the toolkit/pipeline test panels clear a scratch
 *     transcript and keep the shorter "Clear chat". One control, two honest
 *     names, rather than one name that is wrong on one of the two surfaces.
 *   - `testId`   — opt-in `data-testid`. Omitted by default ON PURPOSE: three
 *     different surfaces mount this button, and a single hardcoded id would
 *     resolve to several elements at once wherever two of them are on screen.
 *     The composition root that needs to address one names it.
 */
export interface ClearChatButtonProps {
  disabled?: boolean;
  onClear?: () => void;
  label?: string;
  testId?: string;
}

export const ClearChatButton = memo(({ disabled = false, onClear, label, testId }: ClearChatButtonProps) => {
  const title = label ?? clearChatLabel;
  return (
    <Tooltip title={title} placement="top">
      <Box component="span">
        <IconButton
          color="secondary"
          aria-label={title}
          disabled={disabled}
          onClick={onClear}
          {...(testId !== undefined ? { 'data-testid': testId } : {})}
          sx={{ marginLeft: 0 }}
        >
          <DeleteOutlinedIcon fontSize="small" />
        </IconButton>
      </Box>
    </Tooltip>
  );
});

ClearChatButton.displayName = 'ClearChatButton';
