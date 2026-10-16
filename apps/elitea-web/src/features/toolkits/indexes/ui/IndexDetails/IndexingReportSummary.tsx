import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import { INDEXING_REPORT_KIND_PRESENTATION } from '../../lib/constants/indexingReport.constants';
import {
  categoryHeadline,
  reportHeadline,
  unchangedNotice,
  visibleCategories,
} from '../../lib/helpers/indexingReport.helpers';
import { resolveIndexingReport } from '../../lib/helpers/indexingReport.serialize';

/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/indexing-report/ui/
 * IndexingReportSummary.jsx` — what an index run actually did, broken down
 * by outcome.
 *
 * `source` is deliberately `unknown`: every real caller hands it a raw
 * `index_meta` row or `history` entry straight off the wire (see
 * `../../lib/helpers/indexingReport.serialize.ts` for the shapes), and
 * `resolveIndexingReport` is the one place that knows how to tell those
 * apart. Renders nothing when the source carries no run information — an
 * empty shell would read as "the run reported nothing", which is a
 * different claim.
 */
export interface IndexingReportSummaryProps {
  readonly source: unknown;
  readonly sx?: SxProps<Theme> | undefined;
}

export function IndexingReportSummary(props: IndexingReportSummaryProps): ReactNode {
  const { source, sx } = props;

  const report = useMemo(() => resolveIndexingReport(source), [source]);
  const categories = useMemo(() => (report === null ? [] : visibleCategories(report)), [report]);
  const headline = useMemo(() => (report === null ? null : reportHeadline(report)), [report]);
  const unchanged = useMemo(() => unchangedNotice(report), [report]);

  if (report === null) return null;

  return (
    <Box
      sx={combineSx(rootSx, sx)}
      data-testid="indexing-report-summary"
    >
      {headline !== null && (
        <Typography
          variant="bodyMedium"
          color="text.primary"
        >
          {headline.icon} {headline.text}
        </Typography>
      )}

      {categories.map((category) => {
        const { tone } = INDEXING_REPORT_KIND_PRESENTATION[category.kind];
        const { icon, text } = categoryHeadline(category, report);
        return (
          <Box
            key={category.kind}
            sx={categorySx}
            data-testid={`indexing-report-category-${category.kind}`}
          >
            <Typography
              variant="bodyMedium"
              color={`${tone}.main`}
            >
              {icon} {text}
            </Typography>

            {category.groups.map((group) => (
              <Box
                key={`${group.reason}-${String(group.dependent)}`}
                sx={groupSx}
              >
                <Typography
                  variant="bodySmall"
                  color="text.secondary"
                >
                  {group.label} ({group.count})
                </Typography>
                {group.code !== '' && (
                  <Typography
                    variant="bodySmall"
                    color="text.secondary"
                    title={group.code}
                    sx={itemSx}
                  >
                    {group.code}
                  </Typography>
                )}
                {group.items.map((item, itemIndex) => (
                  <Typography
                    key={`${item}-${String(itemIndex)}`}
                    variant="bodySmall"
                    color="text.secondary"
                    sx={itemSx}
                  >
                    {item}
                  </Typography>
                ))}
                {group.more > 0 && (
                  <Typography
                    variant="bodySmall"
                    color="text.secondary"
                    sx={itemSx}
                  >
                    {t('features.toolkits.indexingReport.andMore', '… and {{count}} more', { count: group.more })}
                  </Typography>
                )}
              </Box>
            ))}
          </Box>
        );
      })}

      {unchanged !== null && (
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          data-testid="indexing-report-unchanged"
        >
          {unchanged.text}
        </Typography>
      )}

      {report.errors.length > 0 && (
        <Box
          sx={categorySx}
          data-testid="indexing-report-errors"
        >
          <Typography
            variant="bodyMedium"
            color="error.main"
          >
            {t('features.toolkits.indexingReport.errors', 'Errors')}
          </Typography>
          {report.errors.map((message, messageIndex) => (
            <Typography
              key={`${message}-${String(messageIndex)}`}
              variant="bodySmall"
              color="text.secondary"
              sx={itemSx}
            >
              {message}
            </Typography>
          ))}
          {report.errorsTotal > report.errors.length && (
            <Typography
              variant="bodySmall"
              color="text.secondary"
              sx={itemSx}
            >
              {t('features.toolkits.indexingReport.andMoreErrors', '… and {{count}} more distinct errors', {
                count: report.errorsTotal - report.errors.length,
              })}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  );
}

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const categorySx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem' };
const groupSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', paddingLeft: '1rem' };
const itemSx: SxProps<Theme> = { paddingLeft: '1rem' };
