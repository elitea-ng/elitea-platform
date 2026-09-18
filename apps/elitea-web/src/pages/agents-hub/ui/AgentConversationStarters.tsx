/* oxlint-disable i18next/no-literal-string -- Wave-2 prototype: UI copy not yet wired through i18n shim (unit S8). REMOVER: S8. */
/**
 * Agent Conversation Starters — renders the "start conversation" buttons.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentConversationStarters.jsx`.
 */
import { memo, useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { AgentConversationStarterItem } from './AgentConversationStarterItem';

export interface AgentConversationStartersProps {
  conversation_starters?: string[];
  onSelectStarter?: (text: string) => void;
}

export const AgentConversationStarters = memo(
  ({ conversation_starters, onSelectStarter }: AgentConversationStartersProps) => {
    // elitea_issues #4932/#4933 — the API's `conversation_starters` array is
    // typed loosely server-side (jsonb passthrough); an agent created/edited
    // through the raw API (or a pre-existing row from before validation
    // existed) can carry a non-string entry (`null`, a number, an object).
    // `s?.trim()` only guards `null`/`undefined` — a number or object still
    // has no `.trim` and threw `TypeError: s.trim is not a function`,
    // crashing this whole panel. `typeof s === 'string'` first makes that
    // structurally unreachable.
    const filtered = useMemo(
      () => (conversation_starters ?? []).filter((s): s is string => typeof s === 'string' && s.trim() !== ''),
      [conversation_starters],
    );

    return (
      <Box sx={styles.container}>
        <Typography variant="subtitle" sx={styles.header}>
          CONVERSATION STARTERS
        </Typography>
        {filtered.length > 0 ? (
          <Box sx={gridSx(filtered.length === 1)}>
            {filtered.map((text, i) => (
              <AgentConversationStarterItem
                key={i}
                text={text}
                {...(onSelectStarter ? { onSelectStarter } : {})}
              />
            ))}
          </Box>
        ) : (
          <Typography variant="bodySmall" sx={styles.empty}>
            No predefined conversation starters – just type your request to begin.
          </Typography>
        )}
      </Box>
    );
  },
);

AgentConversationStarters.displayName = 'AgentConversationStarters';

const styles: Record<string, SxProps<Theme>> = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.75rem',
    width: '100%',
    alignItems: 'center',
  },
  header: { color: 'text.tertiary' },
  empty: { color: 'text.tertiary' },
};

const gridSx = (isSingle: boolean): SxProps<Theme> => ({
  display: 'grid',
  gridTemplateColumns: isSingle ? '1fr' : 'repeat(auto-fill, minmax(15rem, 1fr))',
  gap: '0.5rem',
  width: '100%',
});
