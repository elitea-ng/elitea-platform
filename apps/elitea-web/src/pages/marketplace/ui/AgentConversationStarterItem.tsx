/* oxlint-disable elitea/ad-hoc-radius -- Wave-2 prototype: ad-hoc radius on ported component; replace with theme token when token pass lands. */
/**
 * Agent Conversation Starter Item — individual starter pill/button.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentConversationStarterItem.jsx`.
 */
import { memo, useCallback, useRef } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

export interface AgentConversationStarterItemProps {
  text: string;
  onSelectStarter?: (text: string) => void;
}

export const AgentConversationStarterItem = memo(
  ({ text, onSelectStarter }: AgentConversationStarterItemProps) => {
    const textRef = useRef<HTMLSpanElement>(null);

    const handleClick = useCallback(() => {
      onSelectStarter?.(text);
    }, [onSelectStarter, text]);

    return (
      <Box
        sx={styles.item}
        onClick={handleClick}
      >
        <Typography ref={textRef} variant="bodySmall" sx={styles.text}>
          {text}
        </Typography>
      </Box>
    );
  },
);

AgentConversationStarterItem.displayName = 'AgentConversationStarterItem';

const styles = {
  item: {
    padding: '0.75rem 1rem',
    borderRadius: '1rem',
    backgroundColor: 'background.default',
    cursor: 'pointer',
    minHeight: '2.5rem',
    display: 'flex',
    alignItems: 'center',
    transition: 'background-color 0.2s ease',
    '&:hover': { backgroundColor: 'action.hover' },
  },
  text: {
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    display: '-webkit-box',
    WebkitLineClamp: 2,
    WebkitBoxOrient: 'vertical',
    wordBreak: 'break-word',
  },
};
