/**
 * Admin › Configuration › LLM Proxy — the model catalogue.
 *
 * Bifrost's Model Catalog and Pricing Overrides, over this platform's own
 * tables: `gateway.gateway_models` for the prices and `gateway.llm_usage_events`
 * for the traffic beside them.
 *
 * ## The unpriced list is the point of the screen
 *
 * A (provider, model) pair that was CALLED and has no catalogue row is charged
 * at a figure nobody chose. For a token model the gateway falls back to a prefix
 * table and then to a flat invented rate, so the call IS billed and IS counted —
 * the number is simply wrong. For audio it is billed at nothing at all, because
 * a per-second rate is never fabricated, so no ceiling can stop it.
 *
 * Neither errors and neither warns anywhere else, and both call for the same
 * action: add a row. So the list is rendered first, above the catalogue it is
 * missing from.
 *
 * ## Usage is shown next to price because neither is actionable alone
 *
 * A missing price on a model nobody calls is a triviality; the same gap on the
 * most-called model of the week is an ongoing billing fault. The window selector
 * exists to let an operator tell those apart.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import MenuItem from '@mui/material/MenuItem';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  USAGE_WINDOWS,
  type LlmModelRow,
  type UnpricedLlmModel,
  type UsageWindow,
} from './api/adminLlmProxyApi';
import { monoFontFamily } from '@/shared/brand/typeScale';

/**
 * Renders a price, distinguishing "no rate" from zero.
 *
 * Formatted rather than interpolated raw: these are NUMERIC(20,8), and a small
 * per-1M rate such as 0.00000075 stringifies to "7.5e-7", which reads as
 * corrupt next to a column of ordinary decimals. Trailing zeros are trimmed so
 * a plain 1.25 does not render as 1.25000000.
 */
function priceLabel(value: number | null): string {
  if (value === null) return '—';
  return `$${value.toFixed(8).replace(/\.?0+$/, '')}`;
}

/**
 * Renders a cost with a fixed precision, so a column of them lines up.
 *
 * A non-zero value below the displayed precision renders as "<$0.0001" rather
 * than "$0.0000". This screen exists to separate a model that billed something
 * from one that billed NOTHING — an uncatalogued audio model bills zero and no
 * budget can stop it — and rounding a real sub-cent spend to the same string as
 * a true zero hides exactly that distinction.
 */
function costLabel(value: number): string {
  if (value > 0 && value < 0.0001) return '<$0.0001';
  return `$${value.toFixed(4)}`;
}

/**
 * The called-but-uncatalogued report.
 *
 * Rendered as an error rather than a table section: each row is a model being
 * served for free right now, and the count is usually zero, so an operator who
 * sees this block at all needs to act on it.
 */
export function UnpricedModelsAlert({
  unpriced,
  onPrice,
}: {
  readonly unpriced: readonly UnpricedLlmModel[];
  readonly onPrice: (model: UnpricedLlmModel) => void;
}) {
  if (unpriced.length === 0) return null;
  return (
    <Alert severity="warning" data-testid="llm-proxy-unpriced">
      <Typography variant="bodyMedium" sx={{ fontWeight: 600, marginBottom: '0.25rem' }}>
        {t(
          'pages.admin.llmProxy.models.unpricedTitle',
          '{{count}} model(s) were called with no price in the catalogue',
          { count: unpriced.length },
        )}
      </Typography>
      <Typography variant="bodySmall" sx={{ marginBottom: '0.5rem' }}>
        {t(
          'pages.admin.llmProxy.models.unpricedBody',
          'Text requests to these models are billed at a fallback rate the gateway invents, so the recorded spend is wrong. Audio requests are billed at nothing, so no budget limit can stop them.',
        )}
      </Typography>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
        {unpriced.map((model) => (
          <Box
            key={`${model.provider}:${model.model_name}`}
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              flexWrap: 'wrap',
            }}
          >
            <Typography variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>
              {model.provider} / {model.model_name}
            </Typography>
            <Typography variant="bodySmall" color="text.secondary">
              {t('pages.admin.llmProxy.models.unpricedUsage', '{{requests}} request(s)', {
                requests: model.requests,
              })}
            </Typography>
            <Button
            variant="elitea" color="tertiary" size="small" onClick={() => onPrice(model)}>
              {t('pages.admin.llmProxy.models.setPrice', 'Set a price')}
            </Button>
          </Box>
        ))}
      </Box>
    </Alert>
  );
}

/** The catalogue table. */
export function ModelCatalogueTable({
  items,
  onEdit,
  onClearOverride,
}: {
  readonly items: readonly LlmModelRow[];
  readonly onEdit: (row: LlmModelRow) => void;
  readonly onClearOverride: (row: LlmModelRow) => void;
}) {
  return (
    <TableContainer>
      <Table
        size="small"
        aria-label={t('pages.admin.llmProxy.models.tableLabel', 'Model catalogue')}
        data-testid="llm-proxy-models-table"
      >
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.llmProxy.models.column.provider', 'Provider')}</TableCell>
            <TableCell>{t('pages.admin.llmProxy.models.column.model', 'Model')}</TableCell>
            <TableCell align="right">
              {t('pages.admin.llmProxy.models.column.input', 'Input / 1M')}
            </TableCell>
            <TableCell align="right">
              {t('pages.admin.llmProxy.models.column.output', 'Output / 1M')}
            </TableCell>
            <TableCell align="right">
              {t('pages.admin.llmProxy.models.column.requests', 'Requests')}
            </TableCell>
            <TableCell align="right">
              {t('pages.admin.llmProxy.models.column.cost', 'Cost')}
            </TableCell>
            <TableCell>{t('pages.admin.llmProxy.models.column.source', 'Price source')}</TableCell>
            <TableCell align="right">
              {t('pages.admin.llmProxy.models.column.actions', 'Actions')}
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {items.map((row) => (
            <TableRow key={row.id} hover>
              <TableCell>{row.provider}</TableCell>
              <TableCell sx={{ fontFamily: monoFontFamily }}>{row.model_name}</TableCell>
              <TableCell align="right">{priceLabel(row.input_cost_per_1m_tokens)}</TableCell>
              <TableCell align="right">{priceLabel(row.output_cost_per_1m_tokens)}</TableCell>
              <TableCell align="right">{row.requests}</TableCell>
              <TableCell align="right">{costLabel(row.cost_usd)}</TableCell>
              <TableCell>
                {/* An overridden row is labelled as such, because the label is
                    the only thing on screen that says the automatic sync has
                    stopped updating it. */}
                {row.price_overridden ? (
                  <Chip
                    size="small"
                    color="warning"
                    label={t('pages.admin.llmProxy.models.overridden', 'Override')}
                    title={
                      row.price_overridden_by !== undefined && row.price_overridden_by !== ''
                        ? t('pages.admin.llmProxy.models.overriddenBy', 'Set by {{who}}', {
                            who: row.price_overridden_by,
                          })
                        : undefined
                    }
                  />
                ) : (
                  <Typography variant="bodySmall" color="text.secondary">
                    {row.source !== undefined && row.source !== '' ? row.source : '—'}
                  </Typography>
                )}
              </TableCell>
              <TableCell align="right">
                <Button
            variant="elitea" color="tertiary" size="small" onClick={() => onEdit(row)}>
                  {t('pages.admin.llmProxy.models.edit', 'Edit price')}
                </Button>
                {row.price_overridden ? (
                  <Button
            variant="elitea" color="tertiary" size="small" onClick={() => onClearOverride(row)}>
                    {t('pages.admin.llmProxy.models.clear', 'Resume sync')}
                  </Button>
                ) : null}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

/** The window selector above the table. */
export function UsageWindowSelect({
  usageWindow,
  onChange,
}: {
  // Named `usageWindow`, not `window`: the latter shadows the DOM global inside
  // this function, so a later edit reaching for `window.matchMedia` or
  // `window.location` here would silently be reading a string like '24h'.
  readonly usageWindow: UsageWindow;
  readonly onChange: (next: UsageWindow) => void;
}) {
  return (
    <TextField
      select
      size="small"
      label={t('pages.admin.llmProxy.models.window', 'Usage window')}
      value={usageWindow}
      onChange={(event) => onChange(event.target.value as UsageWindow)}
      sx={{ minWidth: '10rem' }}
      slotProps={{ htmlInput: { 'data-testid': 'llm-proxy-window' } }}
    >
      {USAGE_WINDOWS.map((value) => (
        <MenuItem key={value} value={value}>
          {value}
        </MenuItem>
      ))}
    </TextField>
  );
}
