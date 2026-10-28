/**
 * The small "who answered" caption over a reply in a multi-agent
 * conversation: a 24px circle filled with `background.aiParticipantIcon`
 * carrying the brand mark (what `EntityIcon forMessage` renders for a
 * Dummy/model participant, `ApplicationAnswer.jsx:610-622` +
 * `styles.entityIcon`), then the participant's name in the muted caption
 * colour. Pack-driven via `BrandLogoMark`, so a white-labelled deployment gets
 * its own mark rather than the Elitea orb (ADR-0024 WP3).
 *
 * Only `ChatMessageList` decides whether it shows: a conversation with one
 * assistant needs no name over every reply.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BrandLogoMark } from '@/shared/ui/brand-logo';

const AVATAR_SIZE = '1.5rem';

function circleSx(theme: Theme) {
  return {
    width: AVATAR_SIZE,
    height: AVATAR_SIZE,
    minWidth: AVATAR_SIZE,
    borderRadius: theme.vars.shape.radiusPill,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    overflow: 'hidden',
    flexShrink: 0,
    background: theme.vars.palette.background.aiParticipantIcon,
  } as const;
}

/** The answering participant's mark and name. */
export function AssistantCaption({ name }: { readonly name: string }): ReactNode {
  return (
    <Box data-testid="chat-message-author" sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
      <Box data-testid="chat-message-avatar" aria-hidden sx={circleSx}>
        <BrandLogoMark style={{ width: AVATAR_SIZE, height: AVATAR_SIZE }} />
      </Box>
      <Typography
        variant="bodySmall"
        noWrap
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}
      >
        {name}
      </Typography>
    </Box>
  );
}
