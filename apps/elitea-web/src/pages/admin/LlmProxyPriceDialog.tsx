/**
 * Admin › Configuration › LLM Proxy — the price-override dialog.
 *
 * ## Every field is per 1M units, and the labels say so
 *
 * `gateway.gateway_models` stores prices per 1M tokens (per 1M seconds or
 * characters for the audio dimensions), and the gateway's cost calculator
 * divides by the same 1M. A per-1k number typed into one of these fields is a
 * 1000x costing error that produces no error anywhere — it just bills a
 * thousand times too much or too little — so the unit is in every label rather
 * than in a note above the form.
 *
 * The form offers the six fields the cost path READS and no others; see
 * `./llmProxyPriceForm`.
 *
 * ## Blank means "no rate", not "zero"
 *
 * An empty field is sent as `null`, which means the model has no rate for that
 * dimension. `0` means the dimension is free. They are different, and the
 * distinction is why blank fields are sent rather than omitted: omitting them
 * would leave whatever was previously stored, making it impossible to clear a
 * price that should not exist.
 *
 * ## Saving takes the row off the price sync, permanently
 *
 * A saved price sets `price_overridden`, and the scheduler's price-sync UPSERT
 * skips those rows. That is what makes the edit stick, and it is also a
 * commitment: the row will never track the upstream catalogue again until the
 * override is cleared. The dialog says so, because the consequence outlives the
 * click and nothing else on the screen would reveal it.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  draftHasAnyPrice,
  emptyPriceDraft,
  priceDraftFromRow,
  priceDraftToWrite,
  priceFieldGroups,
  type PriceDraft,
} from './llmProxyPriceForm';
import type { LlmModelPriceDraft, LlmModelRow, UnpricedLlmModel } from './api/adminLlmProxyApi';

export interface LlmProxyPriceDialogProps {
  readonly open: boolean;
  /** The catalogue row being re-priced, when editing one. */
  readonly row: LlmModelRow | undefined;
  /** The called-but-uncatalogued pair being priced, when adding from that list. */
  readonly unpriced: UnpricedLlmModel | undefined;
  readonly saving: boolean;
  readonly error: string | undefined;
  readonly onClose: () => void;
  readonly onSave: (draft: LlmModelPriceDraft) => void;
}

export function LlmProxyPriceDialog({
  open,
  row,
  unpriced,
  saving,
  error,
  onClose,
  onSave,
}: LlmProxyPriceDialogProps) {
  const [draft, setDraft] = useState<PriceDraft>(emptyPriceDraft);

  // Re-seeded whenever the dialog opens on a different subject. Without the
  // `open` dependency a second edit would show the first row's prices, which is
  // the worst possible stale-form bug on a surface that writes billing rates.
  useEffect(() => {
    if (!open) return;
    setDraft(priceDraftFromRow(row, unpriced));
  }, [open, row, unpriced]);

  const identityLocked = row !== undefined || unpriced !== undefined;
  // At least one price is required, mirroring the server's refusal. An override
  // that prices nothing would still mark the row overridden — excluding it from
  // the sync forever while billing it at zero — so the button is disabled rather
  // than letting the operator discover that as a 400.
  const canSave =
    draft.provider.trim() !== '' && draft.model_name.trim() !== '' && draftHasAnyPrice(draft);

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle>
        {row !== undefined
          ? t('pages.admin.llmProxy.price.editTitle', 'Override model price')
          : t('pages.admin.llmProxy.price.addTitle', 'Set model price')}
      </DialogTitle>
      <DialogContent>
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            gap: '1rem',
            paddingTop: '0.5rem',
          }}
        >
          {error !== undefined ? (
            <Alert severity="error" data-testid="llm-proxy-price-error">
              {error}
            </Alert>
          ) : null}

          <Alert severity="info">
            {t(
              'pages.admin.llmProxy.price.overrideNotice',
              'A saved price is an override: the automatic price sync will stop updating this model until the override is cleared.',
            )}
          </Alert>

          <Box sx={{ display: 'flex', gap: '1rem', flexWrap: 'wrap' }}>
            <TextField
              label={t('pages.admin.llmProxy.price.provider', 'Provider')}
              value={draft.provider}
              // Locked when re-pricing: provider and model are the unique key,
              // so editing them here would silently create a SECOND row and
              // leave the original priced as it was.
              disabled={identityLocked || saving}
              onChange={(event) => setDraft((d) => ({ ...d, provider: event.target.value }))}
              size="small"
              sx={{ flex: 1, minWidth: '12rem' }}
              slotProps={{
                htmlInput: {
                  'data-testid': 'llm-proxy-price-provider',
                  maxLength: 64,
                },
              }}
            />
            <TextField
              label={t('pages.admin.llmProxy.price.model', 'Model')}
              value={draft.model_name}
              disabled={identityLocked || saving}
              onChange={(event) => setDraft((d) => ({ ...d, model_name: event.target.value }))}
              size="small"
              sx={{ flex: 1, minWidth: '12rem' }}
              slotProps={{
                htmlInput: {
                  'data-testid': 'llm-proxy-price-model',
                  maxLength: 128,
                },
              }}
            />
          </Box>

          {priceFieldGroups.map((group) => (
            <Box key={group.id} sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              <Typography variant="headingSmall" component="h3">
                {group.label()}
              </Typography>
              <Box sx={{ display: 'flex', gap: '1rem', flexWrap: 'wrap' }}>
                {group.fields.map((field) => (
                  <TextField
                    key={field.key}
                    label={field.label()}
                    value={draft[field.key]}
                    disabled={saving}
                    onChange={(event) =>
                      setDraft((d) => ({ ...d, [field.key]: event.target.value }))
                    }
                    size="small"
                    type="number"
                    // Blank is meaningful: it clears the rate rather than
                    // setting it to zero. The helper text says so on every
                    // field, because the two read identically in an empty box.
                    helperText={t('pages.admin.llmProxy.price.blankHint', 'Blank = no rate')}
                    sx={{ flex: 1, minWidth: '13rem' }}
                    slotProps={{
                      htmlInput: {
                        'data-testid': `llm-proxy-price-${field.key}`,
                        min: 0,
                        step: 'any',
                      },
                    }}
                  />
                ))}
              </Box>
            </Box>
          ))}
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={saving}>
          {t('pages.admin.llmProxy.price.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={!canSave || saving}
          onClick={() => onSave(priceDraftToWrite(draft))}
          data-testid="llm-proxy-price-save"
        >
          {t('pages.admin.llmProxy.price.save', 'Save price')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
