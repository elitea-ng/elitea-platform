/* oxlint-disable i18next/no-literal-string -- Wave-2 prototype: UI copy not yet wired through i18n shim (unit S8). REMOVER: S8. */
/**
 * Agent Welcome Message — displays the agent's welcome text.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentWelcomeMessage.jsx`.
 */
import { memo, useId, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { useLineClampOverflow } from '@/shared/ui/lib/useLineClampOverflow';
import { ShowMoreButton } from '@/shared/ui/ShowMoreButton';

export interface AgentWelcomeMessageProps {
  welcome_message?: string;
}

/**
 * #6861: a long welcome message is clamped to 8 lines. When the clamp cuts
 * it, a "Show more" button opens the full text in place, and "Show less"
 * clamps it again. A message that fits gets no button.
 */
export const AgentWelcomeMessage = memo(({ welcome_message }: AgentWelcomeMessageProps) => {
  const [expanded, setExpanded] = useState(false);
  const { textRef, isOverflowing } = useLineClampOverflow<HTMLSpanElement>(welcome_message);
  const textId = useId();
  const hasMessage = Boolean(welcome_message?.trim());

  return (
    <Box sx={expanded ? styles.containerExpanded : styles.container}>
      <Typography variant="subtitle" sx={styles.header}>
        Welcome Message
      </Typography>
      {hasMessage ? (
        <>
          <Typography
            ref={textRef}
            id={textId}
            variant="bodyMedium"
            sx={expanded ? styles.textExpanded : styles.text}
            data-testid="agent-welcome-message-text"
          >
            {welcome_message}
          </Typography>
          {(isOverflowing || expanded) && (
            <ShowMoreButton
              expanded={expanded}
              controls={textId}
              onClick={() => setExpanded((value) => !value)}
              data-testid="agent-welcome-message-show-more"
              sx={styles.showMore}
            />
          )}
        </>
      ) : (
        <Typography variant="bodySmall" sx={styles.empty}>
          No welcome message set – the agent will start without a greeting.
        </Typography>
      )}
    </Box>
  );
});

AgentWelcomeMessage.displayName = 'AgentWelcomeMessage';

const styles = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.75rem',
    width: '100%',
    flex: '0 1 auto',
    alignItems: 'center',
    maxHeight: '12.5rem',
  },
  containerExpanded: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.75rem',
    width: '100%',
    flex: '0 0 auto',
    alignItems: 'center',
  },
  header: { color: 'text.tertiary', flexShrink: 0 },
  text: {
    color: 'text.secondary',
    width: '100%',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    display: '-webkit-box',
    WebkitBoxOrient: 'vertical',
    wordBreak: 'break-word',
    WebkitLineClamp: 8,
  },
  textExpanded: {
    color: 'text.secondary',
    width: '100%',
    wordBreak: 'break-word',
    whiteSpace: 'pre-wrap',
  },
  showMore: { alignSelf: 'flex-end', flexShrink: 0 },
  empty: { color: 'text.tertiary', textAlign: 'center' },
};
