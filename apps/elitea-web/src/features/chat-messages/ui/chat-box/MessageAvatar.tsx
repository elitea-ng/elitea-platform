/**
 * The 24px avatar that opens a transcript row's caption line.
 *
 * Two shapes, matching the two the production UI renders (measured live on
 * next.elitea.ai at 2000px):
 *
 * - `MessageAvatar` — the human author. A 24×24 circle; the author's picture
 *   when the message carries one (baseline `UserAvatar avatar={avatar}
 *   size={24}`), otherwise the initial over a 10%-white disc, the same
 *   fallback `chat-participants`' own `ParticipantItemRow` draws.
 * - `AssistantAvatar` — the answering participant. A 24×24 circle filled with
 *   `background.aiParticipantIcon` carrying the brand mark, which is what
 *   `EntityIcon forMessage` renders for a Dummy/model participant
 *   (`ApplicationAnswer.jsx:610-622` + `styles.entityIcon`). Pack-driven via
 *   `BrandLogoMark`, so a white-labelled deployment gets its own mark rather
 *   than the Elitea orb (ADR-0024 WP3).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

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
  } as const;
}

/** @public Props for `MessageAvatar`. */
export interface MessageAvatarProps {
  /** The author's name — supplies the initial when there is no picture, and the image's alt text. */
  readonly name: string;
  /** The author's avatar URL, when the message carries one. */
  readonly avatarUrl?: string | undefined;
}

export function MessageAvatar({ name, avatarUrl }: MessageAvatarProps): ReactNode {
  if (avatarUrl !== undefined && avatarUrl !== '') {
    return (
      <Box
        component="img"
        src={avatarUrl}
        alt={name}
        data-testid="chat-message-avatar"
        sx={circleSx}
      />
    );
  }
  return (
    <Box
      data-testid="chat-message-avatar"
      aria-hidden
      sx={(theme: Theme) => ({
        ...circleSx(theme),
        backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
        color: theme.vars.palette.text.secondary,
        fontSize: theme.typography.labelTiny.fontSize,
        fontWeight: 600,
      })}
    >
      {(name?.[0] ?? '?').toUpperCase()}
    </Box>
  );
}

/** The answering participant's avatar — the brand mark on the AI-participant tint. */
export function AssistantAvatar(): ReactNode {
  return (
    <Box
      data-testid="chat-message-avatar"
      aria-hidden
      sx={(theme: Theme) => ({ ...circleSx(theme), background: theme.vars.palette.background.aiParticipantIcon })}
    >
      <BrandLogoMark style={{ width: '1.5rem', height: '1.5rem' }} />
    </Box>
  );
}
