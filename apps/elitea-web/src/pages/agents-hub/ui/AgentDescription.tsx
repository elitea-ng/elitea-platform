/**
 * The description line of the agent catalog modal (`AgentModal`).
 *
 * #6861: on a normal-height window the description is clamped to two lines
 * and a long one ended in "..." with no way to read the rest. When the clamp
 * cuts the text, a "Show more" button now opens it in place, and "Show less"
 * clamps it again. A description that fits gets no button. A short window
 * (`isSmallHeight`) never clamps, so it never needs the button.
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

  return (
    <Box sx={containerSx}>
      <Typography
        ref={textRef}
        id={textId}
        variant="bodySmall"
        sx={descriptionSx(clamped)}
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

const descriptionSx = (clamped: boolean): SxProps<Theme> => ({
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
});
