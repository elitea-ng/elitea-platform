/**
 * The small "who answered" caption over a reply in a multi-agent
 * conversation: the agent's avatar (ADR-0050 decision 6), its name (the
 * persona's `display_name` when the soul sets one) and its persona `role`,
 * in the muted caption colour.
 *
 * The avatar is the agent's own icon; without one it is the brand mark
 * (`BrandLogoMark`, so a white-labelled deployment gets its own mark rather
 * than the Elitea orb, ADR-0024 WP3). Over it, the default motion pack plays
 * the participant's state; the list picks which caption may move.
 *
 * The message class (decision 7e) adds a marker: "temporary" for an
 * ephemeral sub-agent (with a dashed ring), "woke up" for a heartbeat.
 *
 * Only `ChatMessageList` decides whether it shows: a conversation with one
 * assistant needs no name over every reply.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { MESSAGE_CLASS, type AvatarMotion, type MessageClass } from '@elitea/client-core/conversation';

import { t } from '@/shared/i18n';

import { AgentAvatar } from './AgentAvatar';

export interface AssistantCaptionProps {
  readonly name: string;
  /** The persona's `role` (named so as not to read as the ARIA attribute). */
  readonly personaRole?: string | undefined;
  /** The agent's icon URL; absent shows the brand mark. */
  readonly iconUrl?: string | undefined;
  readonly messageClass?: MessageClass | undefined;
  readonly motion?: AvatarMotion | undefined;
}

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

function classMarker(messageClass: MessageClass | undefined): string | undefined {
  if (messageClass === MESSAGE_CLASS.Ephemeral) return t('chatMessages.author.temporary', 'temporary');
  if (messageClass === MESSAGE_CLASS.Heartbeat) return t('chatMessages.author.wokeUp', 'woke up');
  return undefined;
}

/** The answering participant's avatar, name and role. */
export function AssistantCaption({ name, personaRole, iconUrl, messageClass, motion }: AssistantCaptionProps): ReactNode {
  const marker = classMarker(messageClass);
  return (
    <Box
      data-testid="chat-message-author"
      data-message-class={messageClass}
      sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}
    >
      <AgentAvatar iconUrl={iconUrl} motion={motion} ephemeral={messageClass === MESSAGE_CLASS.Ephemeral} />
      <Typography variant="bodySmall" noWrap sx={mutedSx}>
        {name}
      </Typography>
      {personaRole !== undefined && (
        <Typography data-testid="chat-message-author-role" variant="bodySmall" noWrap sx={mutedSx}>
          {`· ${personaRole}`}
        </Typography>
      )}
      {marker !== undefined && (
        <Typography
          data-testid="chat-message-author-marker"
          variant="bodySmall"
          noWrap
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, fontStyle: 'italic' })}
        >
          {marker}
        </Typography>
      )}
    </Box>
  );
}
