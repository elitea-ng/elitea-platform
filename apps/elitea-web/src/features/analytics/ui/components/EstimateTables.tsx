import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import { pickChartColor } from '../../lib/constants';
import { fmtNum, fmtShare, fmtUsd } from '../../lib/format';

/**
 * The four tables the Costs and Tokens tabs draw.
 *
 * They live here for the reason `HealthTables.tsx` does: the two tab files stay
 * under the 400-line budget, and the four tables are one shape with two
 * projections rather than four components.
 *
 * A row's SHARE is computed against the window total the server sent, not
 * against the sum of the rows on screen. The two differ whenever the list was
 * capped (`by_model_truncated`, `by_user_truncated`), and normalising by the
 * visible rows would silently turn "the top 100 models" into "every model",
 * with every percentage inflated to match.
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

const headerRowSx = (theme: Theme) => ({
  display: 'flex',
  padding: `${theme.spacing(1)} ${theme.spacing(1.5)}`,
  borderBottom: `1px solid ${theme.vars.palette.border.table}`,
  gap: theme.spacing(1),
});

const headerCellSx = (theme: Theme) => ({
  fontSize: theme.typography.labelSmall.fontSize,
  fontWeight: 600,
  color: theme.vars.palette.text.metrics,
  textTransform: 'uppercase',
});

const rowSx = (theme: Theme) => ({
  display: 'flex',
  padding: `${theme.spacing(1)} ${theme.spacing(1.5)}`,
  borderBottom: `1px solid ${theme.vars.palette.border.table}`,
  gap: theme.spacing(1),
  '&:hover': { backgroundColor: theme.vars.palette.background.conversation.hover },
});

const cellValueSx = (theme: Theme) => ({
  fontSize: theme.typography.bodyMedium.fontSize,
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});

const emptyTextSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

/**
 * One table row, already reduced to what a cell prints.
 *
 * `cost` is optional and NOT defaulted to zero. A model the price catalogue
 * does not carry has token counts and no money, and a zero in that cell would
 * report it as free.
 */
export interface EstimateRow {
  readonly key: string;
  readonly label: string;
  readonly promptTokens: number;
  readonly completionTokens: number;
  readonly totalTokens: number;
  readonly inputCost?: number | undefined;
  readonly outputCost?: number | undefined;
  readonly totalCost?: number | undefined;
}

export interface EstimateTableProps {
  readonly title: string;
  readonly subtitle: string;
  readonly entityColumn: string;
  readonly rows: readonly EstimateRow[];
  /** The window total the SHARE column divides by. */
  readonly shareTotal: number;
  readonly emptyMessage: string;
}

function TableShell({
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

function EntityCell({ label, index }: { readonly label: string; readonly index: number }): ReactNode {
  const theme = useTheme();
  return (
    <Box sx={combineSx(cellValueSx, { flex: 3, display: 'flex', alignItems: 'center', gap: 1 })}>
      <Box
        sx={{
          width: 8,
          height: 8,
          borderRadius: theme.vars.shape.radiusPill,
          backgroundColor: pickChartColor(index),
          flexShrink: 0,
        }}
      />
      <Typography variant="bodySmall" noWrap>
        {label}
      </Typography>
    </Box>
  );
}

/** The token projection: recorded counts, no money. */
export function TokenTable({
  title,
  subtitle,
  entityColumn,
  rows,
  shareTotal,
  emptyMessage,
}: EstimateTableProps): ReactNode {
  if (rows.length === 0) {
    return (
      <TableShell title={title} subtitle={subtitle}>
        <Typography variant="bodyMedium" sx={emptyTextSx}>
          {emptyMessage}
        </Typography>
      </TableShell>
    );
  }
  return (
    <TableShell title={title} subtitle={subtitle}>
      <Box sx={{ display: 'flex', flexDirection: 'column', width: '100%', overflow: 'auto' }}>
        <Box sx={headerRowSx}>
          <Typography sx={combineSx(headerCellSx, { flex: 3 })}>{entityColumn}</Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.tokens.columnTotal', 'Total Tokens')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.tokens.columnInput', 'Input Tokens')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.tokens.columnOutput', 'Output Tokens')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.estimate.columnShare', 'Share')}
          </Typography>
        </Box>
        {rows.map((row, index) => (
          <Box key={row.key} sx={rowSx}>
            <EntityCell label={row.label} index={index} />
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {fmtNum(row.totalTokens)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {fmtNum(row.promptTokens)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {fmtNum(row.completionTokens)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {fmtShare(row.totalTokens, shareTotal)}
            </Typography>
          </Box>
        ))}
      </Box>
    </TableShell>
  );
}

/**
 * The money projection.
 *
 * A row with no cost prints the unpriced marker in all three money cells. It
 * is deliberately not `$0.00`: the catalogue carries no rate for that model, so
 * what its calls cost is unknown, and an unknown shown as zero makes an
 * under-priced deployment look cheap.
 */
export function CostTable({
  title,
  subtitle,
  entityColumn,
  rows,
  shareTotal,
  emptyMessage,
}: EstimateTableProps): ReactNode {
  const unpriced = t('analytics.costs.unpriced', 'not priced');
  if (rows.length === 0) {
    return (
      <TableShell title={title} subtitle={subtitle}>
        <Typography variant="bodyMedium" sx={emptyTextSx}>
          {emptyMessage}
        </Typography>
      </TableShell>
    );
  }
  return (
    <TableShell title={title} subtitle={subtitle}>
      <Box sx={{ display: 'flex', flexDirection: 'column', width: '100%', overflow: 'auto' }}>
        <Box sx={headerRowSx}>
          <Typography sx={combineSx(headerCellSx, { flex: 3 })}>{entityColumn}</Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.costs.columnTotal', 'Total Cost')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.costs.columnInput', 'Input Token Cost')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.costs.columnOutput', 'Output Token Cost')}
          </Typography>
          <Typography sx={combineSx(headerCellSx, { flex: 1, textAlign: 'right' })}>
            {t('analytics.estimate.columnShare', 'Share')}
          </Typography>
        </Box>
        {rows.map((row, index) => (
          <Box key={row.key} sx={rowSx}>
            <EntityCell label={row.label} index={index} />
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {row.totalCost === undefined ? unpriced : fmtUsd(row.totalCost)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {row.inputCost === undefined ? unpriced : fmtUsd(row.inputCost)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {row.outputCost === undefined ? unpriced : fmtUsd(row.outputCost)}
            </Typography>
            <Typography sx={combineSx(cellValueSx, { flex: 1, textAlign: 'right' })}>
              {row.totalCost === undefined ? unpriced : fmtShare(row.totalCost, shareTotal)}
            </Typography>
          </Box>
        ))}
      </Box>
    </TableShell>
  );
}
