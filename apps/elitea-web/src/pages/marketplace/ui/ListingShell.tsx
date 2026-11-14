/**
 * The frame both listing pages share (ADR-0051 decision 8): the avatar, the
 * name, the current version, the actions, and the page's sections below.
 * A listing is a page with its own URL, not a dialog, so it can be opened in a
 * new tab, bookmarked and shared.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

export interface ListingShellProps {
  readonly name: string;
  readonly iconUrl: string | undefined;
  readonly version: string | undefined;
  /** Beside the name: like, copy link. */
  readonly meta?: ReactNode;
  /** The primary actions: Try in chat, Add to an agent. */
  readonly actions: ReactNode;
  readonly children: ReactNode;
}

export function ListingShell({ name, iconUrl, version, meta, actions, children }: ListingShellProps): React.JSX.Element {
  return (
    <Box data-testid="marketplace-listing" sx={pageSx}>
      <Box component="header" sx={headerSx}>
        {iconUrl !== undefined && <Box component="img" src={iconUrl} alt="" sx={iconSx} />}
        <Box sx={titleSx}>
          <Typography component="h2" variant="headingLarge" data-testid="marketplace-listing-name">
            {name}
          </Typography>
          {version !== undefined && version !== '' && <Chip size="small" label={version} data-testid="marketplace-listing-version" />}
          {meta}
        </Box>
        <Box sx={actionsSx}>{actions}</Box>
      </Box>
      {children}
    </Box>
  );
}

/** One titled block of a listing page. */
export function ListingBlock({ id, title, children }: { readonly id: string; readonly title: string; readonly children: ReactNode }): React.JSX.Element {
  const headingId = `marketplace-block-${id}`;
  return (
    <Box component="section" aria-labelledby={headingId} sx={blockSx}>
      <Typography id={headingId} component="h3" variant="headingSmall" color="text.secondary">
        {title}
      </Typography>
      {children}
    </Box>
  );
}

const pageSx: SxProps<Theme> = {
  height: '100%',
  overflowY: 'auto',
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
  gap: '1.5rem',
  padding: '1.5rem',
  // A page of prose reads best in a column, not across a wide window.
  '& > *': { maxWidth: '56rem', width: '100%' },
};

const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '1rem', flexWrap: 'wrap' };

const iconSx: SxProps<Theme> = (theme: Theme) => ({ width: '3rem', height: '3rem', borderRadius: theme.vars.shape.radiusMd });

const titleSx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' };

const actionsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };

const blockSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
