/**
 * The empty-conversation greeting: brand orb, "Hello, <first name>!", and the
 * "What can I do for you today?" prompt.
 *
 * This is the visible half of a LAYOUT MODE, not just a nicer empty string.
 * With no messages, production does not render an empty transcript with the
 * composer pinned to the bottom of the viewport — it centres the greeting and
 * the composer together as one block in the middle of the chat column, and
 * only switches to transcript-on-top/composer-at-the-bottom once the first
 * turn exists. `ChatBox` owns that switch; this component is what it puts
 * above the composer while the switch is in the empty position.
 *
 * Both lines render as `<p>`, not as headings. `variant="h6"` sets the
 * TYPOGRAPHY; without an explicit `component` MUI also emits an `<h6>`
 * ELEMENT, and on a page whose heading run starts at `<h1>` that is an
 * `heading-order` violation ("heading levels should only increase by one") —
 * which is exactly what axe reported on the `/chat` cold-load journey. A
 * greeting is display text, not a section heading for navigable content, so
 * the right fix is to drop the heading semantics rather than to renumber.
 *
 * The greeting uses the first name only. `user.name` is a full display name
 * ("Alexander Kharkevich"), and the baseline greets by first name; falling
 * back to the whole string when there is no space keeps a single-word name
 * (or a username) working. With no name at all the line is dropped entirely
 * rather than rendered as a half-finished "Hello, !".
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BrandLogoMark } from '@/shared/ui/brand-logo';

export interface ChatEmptyGreetingProps {
  /** The signed-in user's display name, as `ChatBox` received it. */
  readonly userName?: string | undefined;
}

/** "Alexander Kharkevich" -> "Alexander"; "alex" -> "alex"; "" -> undefined. */
function firstNameOf(userName: string | undefined): string | undefined {
  const trimmed = userName?.trim();
  if (!trimmed) return undefined;
  return trimmed.split(/\s+/)[0];
}

export function ChatEmptyGreeting({ userName }: ChatEmptyGreetingProps): ReactNode {
  const name = firstNameOf(userName);

  return (
    <Box
      data-testid="chat-empty-greeting"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        gap: 1.5,
        pb: 3,
      }}
    >
      <BrandLogoMark style={{ width: '3.5rem', height: '3.5rem' }} />
      {name !== undefined && (
        <Typography
          component="p"
          variant="headingLarge"
          sx={{ color: 'primary.main', textAlign: 'center' }}
        >
          {t('widgets.chatBox.greeting', 'Hello, {{name}}!', { name })}
        </Typography>
      )}
      <Typography
        component="p"
        variant="headingMedium"
        sx={{ color: 'text.secondary', textAlign: 'center' }}
      >
        {t('widgets.chatBox.greetingPrompt', 'What can I do for you today?')}
      </Typography>
    </Box>
  );
}
