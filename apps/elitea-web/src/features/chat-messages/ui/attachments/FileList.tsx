/**
 * Ported from `apps/elitea-ui/src/components/Chat/FileList.jsx` — renders
 * a list of file attachments with delete buttons.
 *
 * This component is the consumer of `features/chat-input`'s slot contract:
 * `slots.attachmentList` receives `{ attachments, onDeleteAttachment, disabled }`
 * from `UserInputAttachmentListSlotProps`.
 *
 * Also used by `ChatMessageList` / `ApplicationAnswer` to render the
 * attachment summary inline with message content.
 *
 * Port of `apps/elitea-ui/src/components/Chat/FileList.jsx`.
 */
import type { MouseEvent, ReactNode } from 'react';
import { useCallback, useState } from 'react';

import AttachFileIcon from '@mui/icons-material/AttachFile';
import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import List from '@mui/material/List';
import ListItem from '@mui/material/ListItem';
import ListItemIcon from '@mui/material/ListItemIcon';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';

import type { Attachment } from '@/entities/attachment';
import { t } from '@/shared/i18n';

/**
 * Fixed threshold instead of the baseline's `useGetComponentWidth`
 * measurement (baseline: `FileList.jsx:17-18,50-56` divides the measured
 * container width by a fixed per-item width to compute how many fit) — no
 * container-width-measurement hook exists yet anywhere in `shared/ui`, and a
 * simpler fixed count is an explicitly acceptable substitute for this fix.
 */
const MAX_VISIBLE_ATTACHMENTS = 3;

function attachmentLabel(attachment: Attachment, index: number): string {
  return ((attachment as Record<string, unknown>)?.name as string | undefined) ?? `File ${index + 1}`;
}

function attachmentKey(attachment: Attachment, index: number): React.Key {
  const id = (attachment as Record<string, unknown>)?.id;
  return id != null ? (id as React.Key) : index;
}

/** @public Props for `FileList`. */
export interface FileListProps {
  /** The list of attachments to display. */
  readonly attachments: readonly Attachment[];
  /** Called when a user clicks the delete button on an attachment. */
  readonly onDeleteAttachment?: ((index: number) => void) | undefined;
  /** Disables the delete buttons. */
  readonly disabled?: boolean;
}

/**
 * `FileList` — renders file-name chips for the first `MAX_VISIBLE_ATTACHMENTS`
 * attachments, each with a close button for deletion. Attachments beyond that
 * threshold collapse behind a "+N" chip that opens a `Menu` listing the rest
 * (baseline: `FileList.jsx`'s measured-width overflow menu), so overflowing
 * files stay discoverable instead of only reachable by horizontal scroll.
 */
export function FileList({ attachments, onDeleteAttachment, disabled = false }: FileListProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const open = Boolean(anchorEl);

  const handleMoreClick = useCallback((event: MouseEvent<HTMLElement>) => {
    setAnchorEl(event.currentTarget);
  }, []);

  const handleMenuClose = useCallback(() => setAnchorEl(null), []);

  if (!attachments?.length) return null;

  const visibleAttachments = attachments.slice(0, MAX_VISIBLE_ATTACHMENTS);
  const hiddenAttachments = attachments.slice(MAX_VISIBLE_ATTACHMENTS);

  return (
    <Box sx={{ mb: 1 }}>
      <List
        disablePadding
        sx={{ display: 'flex', gap: 0.5, flexWrap: 'wrap' }}
      >
        {visibleAttachments.map((attachment, index) => (
          <ListItem
            key={attachmentKey(attachment, index)}
            disableGutters
            sx={{ p: 0, width: 'auto' }}
          >
            <Chip
              // Baseline `FileList.jsx`'s own hook for the chips
              // (`chat-attachment-chip-{index}`) — the composer's staged files
              // have no other stable selector, and the E2E journey that guards
              // this wiring has to find them by one.
              data-testid={`chat-attachment-chip-${index}`}
              label={attachmentLabel(attachment, index)}
              size="small"
              variant="outlined"
              sx={{
                maxWidth: '200px',
                '& .MuiChip-label': {
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                },
              }}
              deleteIcon={
                // The testid is OURS, not MUI's. `@mui/icons-material` stamps
                // `data-testid="CloseIcon"` only outside production builds, so
                // a selector on it passes in vitest and finds nothing in the
                // E2E stack's real bundle.
                <CloseIcon
                  fontSize="small"
                  data-testid={`chat-attachment-remove-${index}`}
                  aria-label={t('chatMessages.fileList.removeAttachment', 'Remove attachment')}
                />
              }
              onDelete={
                onDeleteAttachment && !disabled
                  ? () => onDeleteAttachment(index)
                  : undefined
              }
            />
          </ListItem>
        ))}
        {hiddenAttachments.length > 0 && (
          <ListItem
            disableGutters
            sx={{ p: 0, width: 'auto' }}
          >
            <Chip
              data-testid="chat-attachment-overflow-button"
              label={`+${hiddenAttachments.length}`}
              size="small"
              variant="outlined"
              onClick={handleMoreClick}
              aria-haspopup="menu"
              aria-expanded={open ? 'true' : undefined}
              // eslint-disable-next-line i18next/no-literal-string — icon-only chip aria label
              aria-label="Show more files"
            />
          </ListItem>
        )}
      </List>
      {hiddenAttachments.length > 0 && (
        <Menu
          anchorEl={anchorEl}
          open={open}
          onClose={handleMenuClose}
          anchorOrigin={{ vertical: 'top', horizontal: 'right' }}
          transformOrigin={{ vertical: 'bottom', horizontal: 'right' }}
        >
          {hiddenAttachments.map((attachment, index) => {
            const actualIndex = MAX_VISIBLE_ATTACHMENTS + index;
            return (
              <MenuItem
                key={attachmentKey(attachment, actualIndex)}
                data-testid={`chat-attachment-overflow-item-${actualIndex}`}
                sx={{ gap: 1 }}
              >
                <ListItemIcon sx={{ minWidth: 'auto' }}>
                  <AttachFileIcon fontSize="small" />
                </ListItemIcon>
                <Typography
                  variant="bodyMedium"
                  color="text.secondary"
                  sx={{
                    flex: 1,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                    maxWidth: '11.25rem',
                  }}
                >
                  {attachmentLabel(attachment, actualIndex)}
                </Typography>
                {onDeleteAttachment && !disabled && (
                  <IconButton
                    size="small"
                    onClick={(event: MouseEvent<HTMLElement>) => {
                      event.stopPropagation();
                      onDeleteAttachment(actualIndex);
                    }}
                    // eslint-disable-next-line i18next/no-literal-string — icon-only aria label
                    aria-label="Remove attachment"
                  >
                    <CloseIcon fontSize="small" />
                  </IconButton>
                )}
              </MenuItem>
            );
          })}
        </Menu>
      )}
    </Box>
  );
}
