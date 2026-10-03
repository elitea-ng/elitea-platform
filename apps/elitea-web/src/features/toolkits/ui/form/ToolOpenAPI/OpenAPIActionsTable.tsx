import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import KeyboardArrowDownIcon from '@mui/icons-material/KeyboardArrowDown';
import KeyboardArrowRightIcon from '@mui/icons-material/KeyboardArrowRight';
import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import IconButton from '@mui/material/IconButton';
import Paper from '@mui/material/Paper';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TableSortLabel from '@mui/material/TableSortLabel';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { stableSort } from '@/shared/lib/sort';
import type { SortOrder } from '@/shared/lib/sort';
import { ShowMoreButton } from '@/shared/ui/ShowMoreButton';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * ToolOpenAPI/OpenAPIActionsTable.jsx` (426 lines).
 *
 * DISCLOSED SIMPLIFICATIONS:
 *  - `stableSort`/comparator: reuses `shared/lib/sort.ts`'s `stableSort`
 *    (unit S3's port of the same old-app `common/utils.jsx` helper this
 *    baseline file imported) instead of a locally re-implemented copy; this
 *    component's own type-aware, mixed-type-safe `compareValues` dispatch
 *    (string/boolean/number/null-undefined cross-type ordering) is ported
 *    faithfully, wrapped around `stableSort`'s 2-arg `(a, b) => number`
 *    comparator shape instead of the baseline's 3-arg
 *    `(first, second, orderSorting)` shape (`orderSorting` closed over
 *    instead).
 *  - `SortOrderOptions.{ASC,DESC}` (`common/constants.js`) -> the literal
 *    strings `'asc'`/`'desc'`, typed as `shared/lib/sort.ts`'s own
 *    `SortOrder` — same two values, no behaviour change.
 *  - `SortDisabledIcon`/`SortUpwardIcon` (two custom SVGs swapped by
 *    `IconComponent`, purely cosmetic — dims the arrow for a non-active sort
 *    column) have no port anywhere in `shared/ui/icons/` (grepped: neither
 *    exists). `TableSortLabel`'s own default arrow icon (MUI's built-in,
 *    ARIA-correct sort indicator) is used unconditionally instead —
 *    `active`/`direction` are still forwarded, so sorting itself, and which
 *    column visually shows a direction arrow, is unchanged; only the exact
 *    icon asset differs.
 */
export interface OpenAPIAction {
  readonly name: string;
  readonly method: string;
  readonly path?: string;
  readonly description?: string;
}

export interface OpenAPIActionsTableProps {
  readonly tools?: readonly OpenAPIAction[];
  /** Legacy prop support: used when `tools` is empty/omitted. */
  readonly selected_tools?: readonly OpenAPIAction[];
}

type ValueType = 'string' | 'boolean' | 'number' | 'null_undefined';

function getValueType(value: unknown): ValueType {
  if (typeof value === 'string') return 'string';
  if (typeof value === 'boolean') return 'boolean';
  if (typeof value === 'number') return 'number';
  return 'null_undefined';
}

function compareStrings(first: string, second: string, order: SortOrder): number {
  const comparison = first.toLowerCase().localeCompare(second.toLowerCase());
  return order === 'asc' ? comparison : -1 * comparison;
}

function compareNumeric(firstValue: unknown, secondValue: unknown, order: SortOrder, toNumber: (value: unknown) => number): number {
  const comparison = toNumber(firstValue) - toNumber(secondValue);
  return order === 'asc' ? comparison : -1 * comparison;
}

/**
 * Mixed-type comparison, faithfully mirroring the baseline's
 * `comparisonStrategies` table: same-type string/boolean pairs compare by
 * value; a string paired with anything else always sorts first in ascending
 * order (strings are treated as "least"); every other cross-type or
 * same-type-non-string pair falls back to numeric comparison (`??0`-coerced
 * for boolean/null/undefined).
 */
function compareValues(firstValue: unknown, secondValue: unknown, order: SortOrder): number {
  const firstType = getValueType(firstValue);
  const secondType = getValueType(secondValue);

  if (firstType === 'string' && secondType === 'string') {
    return compareStrings(firstValue as string, secondValue as string, order);
  }
  if (firstType === 'string' && secondType !== 'string') {
    return order === 'asc' ? -1 : 1;
  }
  if (firstType !== 'string' && secondType === 'string') {
    return order === 'asc' ? 1 : -1;
  }
  return compareNumeric(firstValue, secondValue, order, (value) => (typeof value === 'boolean' ? (value ? 1 : 0) : ((value as number | undefined) ?? 0)));
}

interface ToolRowProps {
  readonly action: OpenAPIAction;
}

function ToolRow({ action }: ToolRowProps): ReactNode {
  const [isExpanded, setIsExpanded] = useState(false);

  const handleToggleExpand = useCallback(() => setIsExpanded((prev) => !prev), []);

  const rowSx: SxProps<Theme> = useCallback(
    (theme: Theme) => ({
      cursor: 'pointer',
      backgroundColor: isExpanded ? theme.vars.palette.background.secondary : theme.vars.palette.background.default,
      '&:hover': { backgroundColor: theme.vars.palette.background.secondary },
    }),
    [isExpanded],
  );

  const detailsCellSx: SxProps<Theme> = useMemo(() => ({ padding: 0, border: 'none', ...(isExpanded ? {} : { height: 0 }) }), [isExpanded]);

  return (
    <>
      <TableRow
        sx={rowSx}
        onClick={handleToggleExpand}
      >
        <TableCell
          sx={methodCellSx}
          align="left"
        >
          <Box sx={methodCellContentSx}>
            <IconButton
              size="small"
              sx={expandButtonSx}
              onClick={(event) => {
                event.stopPropagation();
                handleToggleExpand();
              }}
            >
              {isExpanded ? <KeyboardArrowDownIcon fontSize="small" /> : <KeyboardArrowRightIcon fontSize="small" />}
            </IconButton>
            <Typography
              component="div"
              sx={methodTextSx}
              variant="bodySmall"
            >
              {action.method}
            </Typography>
          </Box>
        </TableCell>

        <TableCell
          sx={bodyCellSx}
          align="left"
        >
          <Typography
            component="div"
            sx={textSx}
            variant="bodySmall"
          >
            {action.name}
          </Typography>
        </TableCell>
      </TableRow>

      <TableRow>
        <TableCell
          sx={detailsCellSx}
          colSpan={3}
        >
          <Collapse
            in={isExpanded}
            timeout="auto"
            unmountOnExit
          >
            <Box sx={detailsContentSx}>
              {action.description && (
                <Box sx={detailItemSx}>
                  <Typography
                    variant="labelSmall"
                    sx={detailLabelSx}
                  >
                    {t('features.toolkits.openApiActionsTable.descriptionLabel', 'Description:')}
                  </Typography>
                  <Typography
                    variant="bodySmall"
                    sx={detailValueSx}
                  >
                    {action.description}
                  </Typography>
                </Box>
              )}
              {action.path && (
                <Box sx={detailItemSx}>
                  <Typography
                    variant="labelSmall"
                    sx={detailLabelSx}
                  >
                    {t('features.toolkits.openApiActionsTable.pathLabel', 'Path:')}
                  </Typography>
                  <Typography
                    variant="bodySmall"
                    sx={detailValueSx}
                  >
                    {action.path}
                  </Typography>
                </Box>
              )}
            </Box>
          </Collapse>
        </TableCell>
      </TableRow>
    </>
  );
}

const SHOW_MORE_THRESHOLD = 5;

export function OpenAPIActionsTable({ tools = [], selected_tools }: OpenAPIActionsTableProps): ReactNode {
  const availableToolsSource = useMemo(() => (tools.length > 0 ? tools : (selected_tools ?? [])), [tools, selected_tools]);

  const [orderBy, setOrderBy] = useState<keyof OpenAPIAction | ''>('');
  const [order, setOrder] = useState<SortOrder>('asc');
  const [showMore, setShowMore] = useState(false);

  const onClickShowMore = useCallback(() => setShowMore((prev) => !prev), []);

  const sortedActions = useMemo(() => {
    if (!orderBy) return availableToolsSource;
    return stableSort(availableToolsSource, (first, second) => compareValues(first[orderBy], second[orderBy], order));
  }, [order, orderBy, availableToolsSource]);

  const onClickSortLabel = useCallback(
    (fieldName: keyof OpenAPIAction) => () => {
      if (fieldName !== orderBy) {
        setOrderBy(fieldName);
        setOrder('asc');
      } else {
        setOrder((prev) => (prev === 'asc' ? 'desc' : 'asc'));
      }
    },
    [orderBy],
  );

  if (availableToolsSource.length === 0) return null;

  const visibleActions = sortedActions.length <= SHOW_MORE_THRESHOLD || showMore ? sortedActions : sortedActions.slice(0, SHOW_MORE_THRESHOLD);

  return (
    <>
      <TableContainer
        component={Paper}
        sx={tableContainerSx}
      >
        <Table
          stickyHeader
          aria-label={t('features.toolkits.openApiActionsTable.tableAriaLabel', 'tools actions table')}
          size="small"
        >
          <TableHead>
            <TableRow>
              <TableCell
                sx={headCellSx}
                align="left"
              >
                <TableSortLabel
                  sx={sortLabelSx}
                  active
                  direction={orderBy === 'method' ? order : 'desc'}
                  onClick={onClickSortLabel('method')}
                >
                  <Typography variant="labelSmall">{t('features.toolkits.openApiActionsTable.methodColumn', 'Method')}</Typography>
                </TableSortLabel>
              </TableCell>

              <TableCell
                sx={headCellSx}
                align="left"
              >
                <TableSortLabel
                  sx={sortLabelSx}
                  active
                  direction={orderBy === 'name' ? order : 'desc'}
                  onClick={onClickSortLabel('name')}
                >
                  <Typography variant="labelSmall">{t('features.toolkits.openApiActionsTable.endpointColumn', 'Api Endpoint')}</Typography>
                </TableSortLabel>
              </TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {visibleActions.map((action, idx) => (
              <ToolRow
                key={action.name || idx}
                action={action}
              />
            ))}
          </TableBody>
        </Table>
      </TableContainer>
      {sortedActions.length > SHOW_MORE_THRESHOLD && (
        <ShowMoreButton
          expanded={showMore}
          onClick={onClickShowMore}
          moreLabel={t('features.toolkits.openApiActionsTable.showMore', 'Show more')}
          lessLabel={t('features.toolkits.openApiActionsTable.showLess', 'Show less')}
          data-testid="openapi-actions-show-more"
          sx={showMoreSx}
        />
      )}
    </>
  );
}

const expandButtonSx: SxProps<Theme> = (theme: Theme) => ({ padding: '0.125rem', color: theme.vars.palette.text.secondary });
const methodCellContentSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };
const bodyCellSx: SxProps<Theme> = (theme: Theme) => ({
  padding: '0.375rem 0.5rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
  color: theme.vars.palette.text.secondary,
  backgroundColor: theme.vars.palette.background.default,
});
const methodCellSx: SxProps<Theme> = (theme: Theme) => ({
  padding: '0.375rem 0.5rem',
  width: '5rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
  color: theme.vars.palette.text.secondary,
  backgroundColor: theme.vars.palette.background.default,
});
const textSx: SxProps<Theme> = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const methodTextSx: SxProps<Theme> = { textTransform: 'lowercase' };
const detailsContentSx: SxProps<Theme> = (theme: Theme) => ({
  padding: '0.5rem 0.5rem 0.75rem 2.5rem',
  backgroundColor: theme.vars.palette.background.secondary,
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
});
const detailItemSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', marginBottom: '0.5rem', '&:last-child': { marginBottom: 0 } };
const detailLabelSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, marginBottom: '0.125rem' });
const detailValueSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.primary });
const tableContainerSx: SxProps<Theme> = (theme: Theme) => ({
  backgroundColor: theme.vars.palette.background.default,
  boxShadow: 'none',
  border: `0.0625rem solid ${theme.vars.palette.border.table}`,
  borderRadius: theme.vars.shape.radiusSm,
});
// R-T5 bans `!important`: the baseline's own `!important` here exists only
// to beat `MuiTableCell`'s default padding specificity — `sx`'s cascade
// order (applied after the component's own base styles) already wins that
// fight without it.
const headCellSx: SxProps<Theme> = (theme: Theme) => ({
  padding: '0.375rem 0.25rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
  backgroundColor: theme.vars.palette.background.default,
});
const sortLabelSx: SxProps<Theme> = { flexDirection: 'row-reverse' };
const showMoreSx: SxProps<Theme> = { marginTop: '0.625rem' };
