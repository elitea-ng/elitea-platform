/**
 * WebhookFormDialog — create (or edit the url/events/active half of) one
 * outbound project webhook (#876).
 *
 * The secret itself is never editable through this form: creating always
 * mints a fresh one (`generateWebhookSecret`), and rotating an existing
 * webhook's secret is a separate one-click row action
 * (`WebhooksTable.tsx`'s "Rotate secret") — mixing the two into one form
 * would let a url/events edit accidentally also rotate the secret, or vice
 * versa, neither of which the five words on the row's menu promise.
 */
import { useCallback, useEffect, useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

import { WEBHOOK_EVENT_TYPES } from '../../lib/webhooks/webhookEventCatalogue';

export interface WebhookFormValues {
  readonly url: string;
  readonly events: readonly string[];
  readonly active: boolean;
}

export interface WebhookFormDialogProps {
  readonly open: boolean;
  readonly isSaving: boolean;
  /** Present when editing an existing webhook; absent for create. */
  readonly initialValues?: WebhookFormValues | undefined;
  /**
   * The server's own refusal reason for the LAST submit, if it failed —
   * typically the SSRF guard's 400 ("... resolves to a private-network
   * address ...", internal/api/webhook/ssrf.go). Rendered under the URL
   * field, the one input a destination refusal is actually about. Cleared
   * locally the moment the field changes (see the `onChange` handler below):
   * a stale server message surviving an edit would read as still-current
   * feedback on text the user has already changed.
   */
  readonly serverError?: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: WebhookFormValues) => void;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.25rem', minWidth: '25rem' };
const descriptionSx: SxProps<Theme> = { color: 'text.secondary' };

export function WebhookFormDialog({ open, isSaving, initialValues, serverError, onClose, onSubmit }: WebhookFormDialogProps) {
  const [url, setUrl] = useState('');
  const [events, setEvents] = useState<readonly string[]>([]);
  const [active, setActive] = useState(true);
  const [urlError, setUrlError] = useState('');
  // Tracks whether the CURRENT `serverError` value has already been shown
  // and the field edited since — see `serverError`'s own doc comment.
  const [serverErrorDismissed, setServerErrorDismissed] = useState(false);

  // Resets the form to the row being edited (or blank, for create) every time
  // the dialog opens — mirrors `PipelineWebhookModal`'s own open-triggered
  // reset, so a form left half-filled from a previous open never leaks in.
  useEffect(() => {
    if (!open) return;
    setUrl(initialValues?.url ?? '');
    setEvents(initialValues?.events ?? []);
    setActive(initialValues?.active ?? true);
    setUrlError('');
    setServerErrorDismissed(false);
  }, [open, initialValues]);

  // A NEW server refusal (a second failed submit) is shown again even if the
  // previous one was dismissed by editing.
  useEffect(() => {
    setServerErrorDismissed(false);
  }, [serverError]);

  const handleSubmit = useCallback(() => {
    const trimmedUrl = url.trim();
    if (!trimmedUrl) {
      setUrlError(t('entities.webhook.form.urlRequired', 'A destination URL is required'));
      return;
    }
    onSubmit({ url: trimmedUrl, events, active });
  }, [url, events, active, onSubmit]);

  const displayedUrlError = urlError || (!serverErrorDismissed && serverError ? serverError : '');

  const isEdit = initialValues !== undefined;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={isEdit
        ? t('entities.webhook.form.editTitle', 'Edit webhook')
        : t('entities.webhook.form.createTitle', 'New webhook')}
      content={
        <Box sx={contentSx}>
          <Typography variant="bodyMedium" sx={descriptionSx}>
            {t(
              'entities.webhook.form.description',
              'Elitea POSTs a signed request to this URL whenever one of the listed events happens.',
            )}
          </Typography>
          <InputBase
            label={t('entities.webhook.form.urlLabel', 'Destination URL *')}
            placeholder="https://example.com/webhooks/elitea"
            value={url}
            onChange={(event) => {
              setUrl(event.target.value);
              setUrlError('');
              setServerErrorDismissed(true);
            }}
            error={displayedUrlError !== ''}
            helperText={displayedUrlError || undefined}
            data-testid="webhook-form-url"
          />
          <Autocomplete
            multiple
            freeSolo
            options={WEBHOOK_EVENT_TYPES}
            value={events as string[]}
            onChange={(_event, next) => setEvents(next)}
            renderInput={(params) => (
              <TextField
                {...params}
                label={t('entities.webhook.form.eventsLabel', 'Events')}
                placeholder={t('entities.webhook.form.eventsPlaceholder', 'Pick an event, or type your own')}
                helperText={t(
                  'entities.webhook.form.eventsHelp',
                  'Leave blank to fire on every event. Pick from the catalog or type a custom event type.',
                )}
              />
            )}
            data-testid="webhook-form-events"
          />
          <FormControlLabel
            control={(
              <Switch
                checked={active}
                onChange={(event) => setActive(event.target.checked)}
                data-testid="webhook-form-active"
              />
            )}
            label={t('entities.webhook.form.activeLabel', 'Active')}
          />
        </Box>
      }
      actions={{
        confirming: isSaving,
        confirmText: isEdit ? t('entities.webhook.form.save', 'Save') : t('entities.webhook.form.create', 'Create'),
        cancelText: t('entities.webhook.form.cancel', 'Cancel'),
      }}
      onConfirm={handleSubmit}
      data-testid="webhook-form-dialog"
    />
  );
}
