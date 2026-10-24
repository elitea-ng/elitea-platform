/**
 * The "Tools" tab of the admin model pricing page (#1238): the per-unit price
 * of every metered tool operation, editable in place.
 *
 * A tool operation is priced per request, credit or page, never per token, so
 * it is not a row of the model catalogue beside it. The gateway refuses an
 * operation with no price before it calls the provider, and a free operation
 * has an explicit $0, so the table states "no price" rather than hiding a
 * missing row.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { configFailureReason } from './api/adminConfigurationApi';
import { type MeteredPriceRow, useAdminMeteredPrices, useSaveAdminMeteredPrice } from './api/adminMeteredPricesApi';
import { meteredOperationLabel, meteredProviderLabel } from './meteredToolLabels';

/** The column's scale: NUMERIC(20,8). The server refuses a longer fraction. */
const PRICE_PATTERN = /^\d+(\.\d{1,8})?$/;

function unitHeading(unit: string): string {
  switch (unit) {
    case 'credit':
      return t('pages.admin.llmProxy.tools.perCredit', 'per credit');
    case 'page':
      return t('pages.admin.llmProxy.tools.perPage', 'per page');
    default:
      return t('pages.admin.llmProxy.tools.perRequest', 'per request');
  }
}

function PriceRow({ row }: { readonly row: MeteredPriceRow }): ReactNode {
  const save = useSaveAdminMeteredPrice();
  const [draft, setDraft] = useState(String(row.price_per_unit_usd));
  const valid = PRICE_PATTERN.test(draft.trim());
  const changed = valid && Number(draft) !== row.price_per_unit_usd;
  const rowId = `${row.provider}-${row.operation}`;

  return (
    <TableRow data-testid={`metered-price-${rowId}`}>
      <TableCell>{meteredOperationLabel(row.operation)}</TableCell>
      <TableCell>{meteredProviderLabel(row.provider)}</TableCell>
      <TableCell>
        <TextField
          size="small"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          error={!valid}
          helperText={unitHeading(row.unit)}
          slotProps={{ htmlInput: { inputMode: 'decimal', 'aria-label': `${row.provider} ${row.operation} price` } }}
        />
        {save.error != null ? (
          <Typography variant="bodySmall" color="error" sx={{ display: 'block' }}>
            {configFailureReason(save.error) ??
              t('pages.admin.llmProxy.tools.saveError', 'Failed to save the tool price.')}
          </Typography>
        ) : null}
      </TableCell>
      <TableCell>
        {row.price_overridden ? (
          <Chip size="small" label={t('pages.admin.llmProxy.tools.overridden', 'Set by an administrator')} />
        ) : (
          <Chip size="small" variant="outlined" label={t('pages.admin.llmProxy.tools.seeded', 'Default')} />
        )}
      </TableCell>
      <TableCell align="right">
        <Button
          size="small"
          disabled={!changed || save.isPending}
          onClick={() =>
            save.mutate({
              provider: row.provider,
              operation: row.operation,
              unit: row.unit,
              price_per_unit_usd: Number(draft),
            })
          }
        >
          {t('pages.admin.llmProxy.tools.save', 'Save')}
        </Button>
      </TableCell>
    </TableRow>
  );
}

export function LlmProxyToolPricesPanel(): ReactNode {
  const { data, error, isPending } = useAdminMeteredPrices();

  if (isPending) {
    return <LinearProgress aria-label={t('pages.admin.llmProxy.tools.loading', 'Loading tool prices')} />;
  }
  if (error != null) {
    return (
      <Alert severity="error" data-testid="metered-prices-error">
        {configFailureReason(error) ??
          t('pages.admin.llmProxy.tools.loadError', 'The tool prices could not be read.')}
      </Alert>
    );
  }

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProxy.tools.description',
          'USD per unit for web search, web fetch and host lookups. These calls are billed to the same budgets as model calls. An operation without a price is refused before the provider is called; set 0 for a free one.',
        )}
      </Typography>
      {data.length === 0 ? (
        <Alert severity="warning" data-testid="metered-prices-empty">
          {t('pages.admin.llmProxy.tools.empty', 'No tool operation has a price, so every web and host tool call is refused.')}
        </Alert>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small" data-testid="metered-prices">
            <TableHead>
              <TableRow>
                <TableCell>{t('pages.admin.llmProxy.tools.operation', 'Operation')}</TableCell>
                <TableCell>{t('pages.admin.llmProxy.tools.provider', 'Provider')}</TableCell>
                <TableCell>{t('pages.admin.llmProxy.tools.price', 'Price (USD)')}</TableCell>
                <TableCell>{t('pages.admin.llmProxy.tools.source', 'Source')}</TableCell>
                <TableCell />
              </TableRow>
            </TableHead>
            <TableBody>
              {data.map((row) => (
                <PriceRow key={`${row.provider}/${row.operation}`} row={row} />
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Box>
  );
}
