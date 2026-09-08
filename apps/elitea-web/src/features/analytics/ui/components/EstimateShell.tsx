import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

/**
 * The pieces the Costs and Tokens tabs share: a stat tile, the tile strip, the
 * card that wraps a chart, and the two "this deployment cannot answer" states.
 *
 * Split out for the 400-line budget, and because the two absence states are the
 * part most easily got wrong. They say two DIFFERENT things:
 *
 *   - no `estimate` block at all — this database carries no gateway request
 *     log, so neither tab has a source;
 *   - `cost_dimension_available: false` — the log is there and the price
 *     catalogue prices nothing in the window, so tokens answer and money does
 *     not.
 *
 * Collapsing them into one message would tell an operator to look at the wrong
 * thing: the first is a deployment shape, the second is a table an operator
 * fills in on the LLM Proxy screen.
 */

const cardSx = (theme: Theme) => ({
  padding: theme.spacing(2),
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
});

const titleSx = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  marginBottom: theme.spacing(0.5),
  display: 'block',
});

const subtitleSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  fontSize: theme.typography.labelSmall.fontSize,
  marginBottom: theme.spacing(1),
  display: 'block',
});

const tileRowSx = (theme: Theme) => ({
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fit, minmax(11rem, 1fr))',
  gap: theme.spacing(2),
});

const tileLabelSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  fontSize: theme.typography.labelSmall.fontSize,
});

const tileCaptionSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  fontSize: theme.typography.labelSmall.fontSize,
  marginTop: theme.spacing(0.5),
});

const noticeSx = (theme: Theme) => ({
  padding: theme.spacing(2),
  borderRadius: theme.vars.shape.radiusMd,
  border: `1px solid ${theme.vars.palette.border.table}`,
  color: theme.vars.palette.text.metrics,
});

const emptyStateSx = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'center',
  alignItems: 'center',
  gap: theme.spacing(1),
  padding: theme.spacing(8),
  textAlign: 'center',
});

const emptyTextSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

export interface EstimateTile {
  readonly key: string;
  readonly label: string;
  readonly value: string;
  readonly caption: string;
}

/** The tile strip. One tile per figure the response actually carries. */
export function EstimateTiles({ tiles }: { readonly tiles: readonly EstimateTile[] }): ReactNode {
  return (
    <Box sx={tileRowSx}>
      {tiles.map((tile) => (
        <Box key={tile.key} sx={cardSx}>
          <Typography variant="labelSmall" sx={tileLabelSx}>
            {tile.label}
          </Typography>
          <Typography variant="headingMedium">{tile.value}</Typography>
          <Typography variant="labelSmall" sx={tileCaptionSx}>
            {tile.caption}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

/** A titled card, for a chart. */
export function EstimateCard({
  title,
  subtitle,
  children,
}: {
  readonly title: string;
  readonly subtitle: string;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <Box sx={cardSx}>
      <Typography variant="labelMedium" sx={titleSx}>
        {title}
      </Typography>
      <Typography variant="bodySmall" sx={subtitleSx}>
        {subtitle}
      </Typography>
      {children}
    </Box>
  );
}

/** A one-line statement about the figures below it. */
export function EstimateNotice({ children }: { readonly children: ReactNode }): ReactNode {
  return (
    <Box sx={noticeSx}>
      <Typography variant="bodySmall">{children}</Typography>
    </Box>
  );
}

/**
 * What both tabs render when this database has no gateway request log.
 *
 * `tabIndex={0}` and the column layout are not decoration: a two-line block
 * inside a scrollable parent must be reachable from the keyboard, which is what
 * axe's `scrollable-region-focusable` rule checks in the E2E run.
 */
export function EstimateUnavailable({ detail }: { readonly detail: string }): ReactNode {
  return (
    <Box sx={emptyStateSx} tabIndex={0}>
      <Typography variant="bodyMedium" sx={emptyTextSx}>
        {t('analytics.unavailable.title', 'Not available on this deployment')}
      </Typography>
      <Typography variant="bodySmall" sx={combineSx(emptyTextSx, { overflowWrap: 'anywhere' })}>
        {detail}
      </Typography>
    </Box>
  );
}
