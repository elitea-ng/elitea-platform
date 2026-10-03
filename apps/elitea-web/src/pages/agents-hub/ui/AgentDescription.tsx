/**
 * The description line of the agent catalog modal (`AgentModal`).
 *
 * #6861: on a normal-height window the description is clamped to two lines
 * and a long one ended in "..." with no way to read the rest. When the clamp
 * cuts the text, a "Show more" button now opens it in place, and "Show less"
 * clamps it again. A description that fits gets no button. A short window
 * (`isSmallHeight`) never clamps, so it never needs the button.
 *
 * The modal content clips on a normal-height window, so the opened text is
 * capped at `EXPANDED_DESCRIPTION_MAX_HEIGHT` and scrolls; the button sits
 * outside that scroll box and is always reachable.
 */
import { useId, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useLineClampOverflow } from '@/shared/ui/lib/useLineClampOverflow';
import { ShowMoreButton } from '@/shared/ui/ShowMoreButton';

export interface AgentDescriptionProps {
  readonly description: string;
  readonly isSmallHeight: boolean;
}

export function AgentDescription({ description, isSmallHeight }: AgentDescriptionProps): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const { textRef, isOverflowing } = useLineClampOverflow<HTMLSpanElement>(description);
  const textId = useId();
  const clamped = !isSmallHeight && !expanded;
  // On a normal-height window the modal content clips (`overflow: hidden`),
  // so an opened description scrolls inside its own bounded box: the rest of
  // the text and the "Show less" button below it stay reachable.
  const bounded = !isSmallHeight && expanded;

  return (
    <Box sx={containerSx}>
      <Typography
        ref={textRef}
        id={textId}
        variant="bodySmall"
        sx={descriptionSx(clamped, bounded)}
        data-testid="agent-modal-description"
      >
        {description}
      </Typography>
      {!isSmallHeight && (isOverflowing || expanded) && (
        <ShowMoreButton
          expanded={expanded}
          controls={textId}
          onClick={() => setExpanded((value) => !value)}
          data-testid="agent-modal-description-show-more"
        />
      )}
    </Box>
  );
}

const containerSx: SxProps<Theme> = {
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: '0.25rem',
  flexShrink: 0,
};

/** The tallest an opened description grows before it scrolls (about eight lines). */
const EXPANDED_DESCRIPTION_MAX_HEIGHT = '10rem';

const descriptionSx = (clamped: boolean, bounded: boolean): SxProps<Theme> => ({
  textAlign: 'center',
  color: 'text.metrics',
  width: '100%',
  wordBreak: 'break-word',
  ...(clamped
    ? {
        height: '2.5rem',
        display: '-webkit-box',
        WebkitLineClamp: 2,
        WebkitBoxOrient: 'vertical',
        overflow: 'hidden',
      }
    : { whiteSpace: 'pre-wrap' }),
  ...(bounded ? { maxHeight: EXPANDED_DESCRIPTION_MAX_HEIGHT, overflowY: 'auto' } : {}),
});
