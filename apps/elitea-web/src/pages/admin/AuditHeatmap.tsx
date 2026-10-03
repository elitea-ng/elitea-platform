/**
 * The activity heatmap above the audit tables: time buckets across, duration
 * bands down, event (or trace) counts in the cells. Clicking a cell drills the
 * tables below into that one bucket × band.
 *
 * ## Not a chart library
 *
 * The reference renders this with `@nivo/heatmap`. elitea-web does not depend
 * on nivo — its one charting dependency is `recharts`, which has no heatmap
 * mark — so this is a CSS grid instead of a third dependency for one view.
 * That turns out to be the better trade beyond bundle size: every cell is a
 * real `<button>` with its own accessible name, so the drill-down is reachable
 * by keyboard and assertable in a test, neither of which is true of nivo's
 * canvas/SVG cells.
 *
 * Colour is `primary.main` at a computed alpha rather than a sequential ramp of
 * hex stops: `elitea/no-raw-color` (R-T1) forbids colour literals outside the
 * token package, and an opacity ramp over one token is theme-aware in both
 * schemes without an `elitea/no-mode-branch` violation (the reference picks a
 * different palette per `theme.palette.mode`, which this app's lint bans).
 */
import { memo, useMemo } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import LinearProgress from '@mui/material/LinearProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { AuditHeatmap as AuditHeatmapData, AuditViewMode } from './api/adminAuditApi';
import { findDurationBand, formatBucket } from './auditFormat';
import type { AuditCellFilter } from './useAdminAuditTrailPage';

/** At most this many column labels, thinned evenly — more than this overlaps. */
const MAX_TICK_LABELS = 12;

/**
 * The faintest a populated cell may be drawn. Without a floor, a bucket holding
 * one event next to a bucket holding ten thousand is indistinguishable from an
 * empty one — the busiest hour would erase every other hour on the chart.
 */
const MIN_CELL_ALPHA = 0.18;

export interface AuditHeatmapProps {
  readonly heatmap: AuditHeatmapData | undefined;
  readonly isFetching: boolean;
  readonly viewMode: AuditViewMode;
  readonly onCellSelect: (cell: AuditCellFilter) => void;
}

interface HeatmapCell {
  readonly bucket: number;
  readonly count: number | null;
  readonly alpha: number;
}

interface HeatmapRow {
  readonly bandLabel: string;
  readonly cells: readonly HeatmapCell[];
}

/**
 * Square-root scaling, not linear: audit volume is heavily skewed (one busy
 * minute per quiet hour is normal), and a linear ramp against the maximum
 * flattens everything that is not the peak into the same near-invisible tint.
 */
function cellAlpha(count: number | null, max: number): number {
  if (count === null || count <= 0 || max <= 0) return 0;
  return MIN_CELL_ALPHA + (1 - MIN_CELL_ALPHA) * Math.sqrt(count / max);
}

function buildRows(heatmap: AuditHeatmapData): { rows: HeatmapRow[]; buckets: number[]; max: number } {
  let max = 0;
  for (const series of heatmap.series) {
    for (const point of series.data) {
      if (point.y !== null && point.y > max) max = point.y;
    }
  }
  const buckets = heatmap.series[0]?.data.map((point) => point.x) ?? [];
  const rows = heatmap.series.map((series) => ({
    bandLabel: series.id,
    cells: series.data.map((point) => ({
      bucket: point.x,
      count: point.y,
      alpha: cellAlpha(point.y, max),
    })),
  }));
  return { rows, buckets, max };
}

/**
 * Column labels, thinned to `MAX_TICK_LABELS`. Returns one entry per bucket so
 * the label row stays in grid alignment with the cells; the thinned-out ones
 * are empty strings rather than missing columns.
 */
function buildTickLabels(buckets: readonly number[], intervalSeconds: number, rangeSeconds: number): string[] {
  const step = Math.max(1, Math.ceil(buckets.length / MAX_TICK_LABELS));
  return buckets.map((bucket, index) =>
    index % step === 0 ? formatBucket(bucket, intervalSeconds, rangeSeconds) : '',
  );
}

const gridSx = (columns: number) => ({
  display: 'grid',
  gridTemplateColumns: `auto repeat(${columns}, minmax(0, 1fr))`,
  gap: '0.0625rem',
  alignItems: 'stretch',
});

const cellSx = (theme: Theme, alpha: number) => ({
  height: '1.5rem',
  width: '100%',
  minWidth: 0,
  borderRadius: theme.vars.shape.radiusSm,
  backgroundColor: theme.vars.palette.primary.main,
  opacity: alpha,
  transition: 'outline-color 0.15s ease-in-out',
  outline: '0.125rem solid transparent',
  outlineOffset: '-0.125rem',
  '&:hover, &:focus-visible': {
    outlineColor: theme.vars.palette.text.secondary,
  },
});

const emptyCellSx = (theme: Theme) => ({
  height: '1.5rem',
  width: '100%',
  minWidth: 0,
  borderRadius: theme.vars.shape.radiusSm,
  backgroundColor: theme.vars.palette.background.userInputBackground,
});

export const AuditHeatmap = memo(function AuditHeatmap({
  heatmap,
  isFetching,
  viewMode,
  onCellSelect,
}: AuditHeatmapProps) {
  const model = useMemo(() => (heatmap ? buildRows(heatmap) : null), [heatmap]);
  const metadata = heatmap?.metadata ?? null;

  const tickLabels = useMemo(
    () =>
      model && metadata
        ? buildTickLabels(model.buckets, metadata.interval_seconds, metadata.range_seconds)
        : [],
    [model, metadata],
  );

  // Nothing to draw. Rendered as an explicit empty state rather than as a
  // collapsed chart, so "the range is quiet" never looks like "the chart broke".
  if (!model || model.buckets.length === 0 || !metadata) {
    return (
      <Box sx={{ paddingBlock: '0.5rem' }}>
        {isFetching ? <LinearProgress /> : null}
        <Typography variant="bodyMedium" color="text.secondary">
          {t('pages.admin.audit.heatmap.empty', 'No activity to chart for this range.')}
        </Typography>
      </Box>
    );
  }

  const unitLabel =
    viewMode === 'traces'
      ? t('pages.admin.audit.heatmap.traces', 'traces')
      : t('pages.admin.audit.heatmap.events', 'events');

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }} data-testid="audit-heatmap">
      <Box sx={{ height: '0.25rem' }}>{isFetching ? <LinearProgress /> : null}</Box>

      <Typography variant="bodySmall" color="text.secondary">
        {`${metadata.total} ${unitLabel} · ${metadata.interval_label} ${t('pages.admin.audit.heatmap.buckets', 'buckets')}`}
      </Typography>

      <Box sx={gridSx(model.buckets.length)}>
        {model.rows.map((row) => (
          <HeatmapBandRow
            key={row.bandLabel}
            row={row}
            unitLabel={unitLabel}
            intervalSeconds={metadata.interval_seconds}
            rangeSeconds={metadata.range_seconds}
            onCellSelect={onCellSelect}
          />
        ))}

        {/* Column labels, in the same grid so they stay aligned with the cells. */}
        <Box />
        {tickLabels.map((label, index) => (
          <Typography
            // The bucket epoch is the stable identity here; the label repeats.
            key={model.buckets[index]}
            variant="bodySmall"
            color="text.secondary"
            sx={{ whiteSpace: 'nowrap', overflow: 'visible' }}
          >
            {label}
          </Typography>
        ))}
      </Box>
    </Box>
  );
});

interface HeatmapBandRowProps {
  readonly row: HeatmapRow;
  readonly unitLabel: string;
  readonly intervalSeconds: number;
  readonly rangeSeconds: number;
  readonly onCellSelect: (cell: AuditCellFilter) => void;
}

const HeatmapBandRow = memo(function HeatmapBandRow({
  row,
  unitLabel,
  intervalSeconds,
  rangeSeconds,
  onCellSelect,
}: HeatmapBandRowProps) {
  const band = findDurationBand(row.bandLabel);

  return (
    <>
      <Typography
        variant="bodySmall"
        color="text.secondary"
        sx={{ whiteSpace: 'nowrap', paddingInlineEnd: '0.5rem', alignSelf: 'center' }}
      >
        {row.bandLabel}
      </Typography>

      {row.cells.map((cell) => {
        const timeLabel = formatBucket(cell.bucket, intervalSeconds, rangeSeconds);

        // An empty cell is not a button: there is nothing to drill into, and a
        // grid of thousands of no-op buttons is both a tab-order hazard and the
        // "control that does nothing" defect at scale.
        if (cell.count === null || !band) {
          return <Box key={cell.bucket} sx={emptyCellSx} />;
        }

        const label = `${timeLabel} · ${row.bandLabel} · ${cell.count} ${unitLabel}`;
        return (
          <Tooltip key={cell.bucket} title={label} placement="top">
            <ButtonBase
              aria-label={label}
              sx={(theme) => cellSx(theme, cell.alpha)}
              onClick={() =>
                onCellSelect({
                  // The bucket's own width, taken from the server's metadata
                  // rather than guessed — it is what makes the drill-down land
                  // on exactly the rows the cell counted.
                  dateFrom: new Date(cell.bucket * 1000),
                  dateTo: new Date((cell.bucket + intervalSeconds) * 1000),
                  bandLabel: row.bandLabel,
                  timeLabel,
                  durationMin: band.min,
                  durationMax: band.max,
                })
              }
            />
          </Tooltip>
        );
      })}
    </>
  );
});
