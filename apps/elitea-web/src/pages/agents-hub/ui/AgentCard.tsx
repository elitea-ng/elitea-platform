/* oxlint-disable i18next/no-literal-string -- Wave-2 prototype: UI copy not yet through i18n shim (S8). REMOVER: S8. */
/**
 * Agent Card — displays a single agent in the hub grid.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentCard.jsx`.
 *
 * Deviations:
 *  - No tour IDs (A13 §6.6: tour targets dropped).
 *  - No EntityIcon/AuthorContainer — simplified with Box + img.
 */
import { memo, useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import Card from '@mui/material/Card';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import AgentHubLike from './AgentHubLike';
import type { ApplicationData, AuthorData } from '../types';

export interface AgentCardProps {
  application: ApplicationData;
  onSelectItem?: ((app: ApplicationData) => void) | undefined;
}

const AgentCard = memo(({ application, onSelectItem }: AgentCardProps) => {
  const handleClick = useCallback(() => {
    onSelectItem?.(application);
  }, [application, onSelectItem]);

  const authors = useMemo(() => {
    const { authors = [], author = {} as AuthorData } = application;
    return !authors?.length ? (author?.id ? [author] : []) : authors;
  }, [application]);

  if (!application) return null;

  return (
    <Card sx={styles.card} onClick={handleClick}>
      <Box sx={styles.header}>
        {application.icon_meta && (
          <Box
            component="img"
            src={(application.icon_meta as Record<string, string>).url}
            alt={application.name}
            sx={styles.icon}
          />
        )}
        <Typography variant="headingSmall" sx={styles.title}>
          {application.name || 'Untitled'}
        </Typography>
      </Box>
      <Box sx={styles.footer}>
        {authors.length > 0 && authors[0]?.name && (
          <Typography variant="bodySmall" sx={styles.author}>
            {authors[0].name}
          </Typography>
        )}
        <AgentHubLike data={application} />
      </Box>
    </Card>
  );
});

AgentCard.displayName = 'AgentCard';

const styles: Record<string, SxProps<Theme>> = {
  card: {
    height: '7rem',
    maxHeight: '7rem',
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    flexGrow: 0,
    boxSizing: 'border-box',
    paddingBottom: 0,
    cursor: 'pointer',
    boxShadow: 'none',
  },
  header: {
    display: 'flex',
    flexDirection: 'row',
    alignItems: 'center',
    gap: '1rem',
    width: '100%',
    padding: '0.75rem 1.25rem',
    height: '4.5rem',
  },
  icon: {
    width: '2rem',
    height: '2rem',
    flexShrink: 0,
  },
  title: {
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    display: '-webkit-box',
    WebkitLineClamp: 2,
    WebkitBoxOrient: 'vertical',
  },
  footer: {
    display: 'flex',
    alignItems: 'center',
    width: '100%',
    height: '2.5rem',
    justifyContent: 'space-between',
    padding: '0 1rem 0.75rem 1.25rem',
    gap: '0.25rem',
  },
  author: {
    color: 'text.secondary',
    fontSize: (theme: Theme) => theme.typography.bodySmall.fontSize,
  },
};

export default AgentCard;
