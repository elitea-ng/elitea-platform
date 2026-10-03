/**
 * ui/playback/PlaybackToolBar.tsx — playback navigation toolbar with
 * forward/backward buttons and a message input display area, ported
 * from `apps/elitea-ui/src/pages/NewChat/PlaybackToolBar.jsx` (C4 batch).
 *
 * Shows left/right navigation arrows, a display area for the current
 * message content (with attachment chips), and a thinking indicator.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { Box, IconButton, Typography } from '@mui/material';
import type { Theme } from '@mui/material/styles';

const IconButtonAny = IconButton as React.ComponentType<any>;

export interface PlaybackToolBarAttachment {
  readonly name: string;
  readonly id: string | number;
}

export interface PlaybackToolBarProps {
  /** Called when the user clicks forward. */
  readonly onForward?: () => void;
  /** Called when the user clicks backward. */
  readonly onBackward?: () => void;
  /** When true, the backward button is disabled. */
  readonly disableBackward?: boolean;
  /** When true, the forward button is disabled. */
  readonly disableForward?: boolean;
  /** The current message being displayed (used to extract attachments and content). */
  readonly message?: Record<string, unknown>;
  /** MUI sx overrides for the toolbar container. */
  readonly sx?: Record<string, unknown>;
  /** When true, show a thinking/loading indicator on the forward button. */
  readonly isMockingThinking?: boolean;
  /** Attachments extracted from the current message. */
  readonly attachments?: readonly PlaybackToolBarAttachment[];
}

/**
 * Renders the playback toolbar — forward/backward navigation with a
 * message display area in the middle.
 *
 * Matches the baseline `PlaybackToolBar.jsx` layout:
 * - Left arrow button (backward)
 * - Center: attachment chips (if present) + message content display
 * - Right arrow button (forward) + thinking indicator
 * - Keyboard shortcuts: Left arrow = backward, Right arrow = forward
 */
export function PlaybackToolBar({
  sx,
  onForward,
  onBackward,
  disableBackward = false,
  disableForward = false,
  message,
  isMockingThinking = false,
  attachments,
}: PlaybackToolBarProps): React.ReactElement {
  const [rows, setRows] = useState(15);
  const textInputRef = useRef<HTMLDivElement>(null);
  const [showExpandIcon, setShowExpandIcon] = useState(false);

  // Determine max rows for the message content display
  const MAX_ROWS = 15;
  const MIN_ROWS = 3;
  const MIN_HEIGHT = 70;

  const onClickExpander = useCallback(() => {
    setRows((prevRows) => (prevRows === MAX_ROWS ? MIN_ROWS : MAX_ROWS));
  }, []);

  const onClickForward = useCallback(() => {
    if (!disableForward && !isMockingThinking) {
      onForward?.();
    }
  }, [disableForward, isMockingThinking, onForward]);

  const onClickBackward = useCallback(() => {
    if (!disableBackward) {
      onBackward?.();
    }
  }, [disableBackward, onBackward]);

  // Keyboard shortcuts
  const onKeyDown = useCallback(
    (event: KeyboardEvent) => {
      if (event.key === 'ArrowLeft') {
        event.preventDefault();
        onClickBackward();
      } else if (event.key === 'ArrowRight') {
        event.preventDefault();
        onClickForward();
      }
    },
    [onClickBackward, onClickForward],
  );

  useEffect(() => {
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [onKeyDown]);

  // Show expand/collapse icon when the content overflows
  useEffect(() => {
    if ((textInputRef.current?.offsetHeight ?? 0) > MIN_HEIGHT) {
      setShowExpandIcon(true);
      setRows(MAX_ROWS);
    } else {
      setShowExpandIcon(false);
    }
  }, []);

  // Extract content from the message
  const rawItems = message?.message_items as unknown[] | undefined;
  const itemsFiltered = rawItems?.filter((item: unknown) => {
    const itemRec = item as Record<string, unknown>;
    return itemRec?.item_type !== 'attachment_message';
  }) ?? [];
  const itemsMapped = itemsFiltered.map((item: unknown) => {
    const itemRec = item as Record<string, unknown>;
    const details = itemRec?.item_details as Record<string, unknown> | undefined;
    return (details?.content as string) ?? '';
  });
  const itemsFinal = itemsMapped.filter((c): c is string => c.length > 0);

  const messageContent: string = (message?.content || itemsFinal.join(', ')) as string;

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'row',
        alignItems: 'center',
        gap: '0.5rem',
        ...sx,
      }}
    >
      {/* Backward button */}
      <IconButtonAny
        variant="elitea"
        color="secondary"
        size="small"
        onClick={onClickBackward}
        disabled={disableBackward}
        sx={{ width: '30px', height: '100%', cursor: disableBackward ? 'default' : 'pointer' }}
        aria-label="Backward"
      >
        ◀
      </IconButtonAny>

      {/* Content area */}
      <Box
        sx={{
          flex: 1,
          marginRight: '0.25rem',
          flexDirection: 'column',
          gap: '1.5rem',
          overflow: 'auto',
        }}
      >
        {/* Attachment chips */}
        {attachments && attachments.length > 0 && (
          <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
            {attachments.map((att: PlaybackToolBarAttachment) => (
              <Box
                key={att.id}
                sx={{
                  padding: '0.25rem 0.5rem',
                  borderRadius: '0.25rem',
                  backgroundColor: 'action.selected',
                  fontSize: (theme: Theme) => theme.typography.bodySmall.fontSize,
                  whiteSpace: 'nowrap',
                }}
              >
                {att.name}
              </Box>
            ))}
          </Box>
        )}

        {/* Message content display */}
        <Box
          ref={textInputRef}
          sx={{
            color: 'text.secondary',
            padding: '0px',
            '& textarea': {
              marginBottom: '0px',
            },
          }}
        >
          <Typography
            variant="bodyMedium"
            sx={{
              overflow: 'auto',
              whiteSpace: 'pre-wrap',
              maxHeight: `${rows * 1.5}rem`,
              color: 'text.secondary',
              WebkitTextFillColor: 'text.secondary',
            }}
          >
            {messageContent ? messageContent.trim() : ''}
          </Typography>
          {showExpandIcon && (
            <IconButtonAny
              variant="elitea"
              color="tertiary"
              size="small"
              onClick={onClickExpander}
              sx={{ width: '1rem', height: '1rem' }}
              aria-label="Expand/collapse"
            >
              {rows === MAX_ROWS ? '▲' : '▼'}
            </IconButtonAny>
          )}
        </Box>
      </Box>

      {/* Forward button */}
      <Box sx={{ display: 'flex', alignItems: 'center' }}>
        <IconButtonAny
          variant="elitea"
          color="secondary"
          size="small"
          onClick={onClickForward}
          disabled={disableForward || isMockingThinking}
          sx={{ width: '30px', cursor: disableForward || isMockingThinking ? 'default' : 'pointer' }}
          aria-label="Forward"
        >
          ▶
        </IconButtonAny>
        {isMockingThinking && (
          <Box
            component="span"
            sx={{
              width: '16px',
              height: '16px',
              border: '2px solid',
              borderColor: 'primary.main',
              borderTopColor: 'transparent',
              borderRadius: '50%',
              animation: 'spin 1s linear infinite',
              marginLeft: '0.5rem',
            }}
          />
        )}
      </Box>
    </Box>
  );
}
