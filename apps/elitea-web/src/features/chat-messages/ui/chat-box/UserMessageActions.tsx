/**
 * The hover-revealed Copy/Edit/Delete action row of a `UserMessage` bubble —
 * split out of `UserMessage.tsx` verbatim to keep that file under the §3.5
 * file-length-400 budget once the attachment `projectId`/error-alert wiring
 * landed there (same sibling-split convention as
 * `widgets/chat`'s `PlusChatButton.parts.tsx`). Not exported from this
 * feature's `index.ts` barrel: `UserMessage` is its only consumer.
 */
import type { ReactNode } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

/** Each button only renders when its handler is supplied. */
export function UserMessageActions({
  onCopy,
  onEdit,
  onDelete,
}: {
  readonly onCopy?: (() => void) | undefined;
  readonly onEdit?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
}): ReactNode {
  return (
    // baseline `RelativeButtonsContainer`: right-aligned inside the card,
    // 0.5rem gap, 8px above, and always visible (see the sibling
    // `ApplicationAnswerActions` note).
    <Box className="actionButtons" sx={{ display: 'flex', justifyContent: 'flex-end', alignItems: 'flex-start', gap: 1, mt: 1, paddingLeft: '2rem' }}>
      {onCopy && (
        // eslint-disable-next-line i18next/no-literal-string — tooltip label
        <Tooltip title="Copy to clipboard" placement="top">
          <IconButton
            size="small"
            color="tertiary"
            onClick={onCopy}
            // eslint-disable-next-line i18next/no-literal-string — accessible name
            aria-label="Copy to clipboard"
          >
            <ContentCopyIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      {onEdit && (
        // eslint-disable-next-line i18next/no-literal-string — tooltip label
        <Tooltip title="Edit the message and regenerate answer" placement="top">
          <IconButton
            size="small"
            color="tertiary"
            onClick={onEdit}
            // eslint-disable-next-line i18next/no-literal-string — accessible name
            aria-label="Edit the message and regenerate answer"
          >
            <EditOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      {onDelete && (
        // eslint-disable-next-line i18next/no-literal-string — tooltip label
        <Tooltip title="Delete" placement="top">
          <IconButton
            size="small"
            color="tertiary"
            onClick={onDelete}
            // eslint-disable-next-line i18next/no-literal-string — accessible name
            aria-label="Delete"
          >
            <DeleteOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
    </Box>
  );
}
