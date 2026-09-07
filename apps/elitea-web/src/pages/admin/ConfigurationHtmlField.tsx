/**
 * The editor for a `format: "html"` configuration field — today, the
 * maintenance splash body.
 *
 * A textarea and, beside it, the SANITISED render of what is in it. The preview
 * is the feature, not decoration: the field's output is injected into the page
 * of every user a maintenance window refuses, and the two things an operator
 * needs to know before saving are what it will look like and what the platform
 * will strip. A textarea alone tells them neither until the window is already
 * open.
 *
 * The preview renders through `sanitizeSplashHtml`, which is the SAME function
 * the splash itself renders through — so the preview cannot show something the
 * live page would not, which would be worse than no preview at all.
 */
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { sanitizeSplashHtml } from '@/shared/ui/lib/sanitizeSplashHtml';
import type { AdminConfigField } from './api/adminConfigurationApi';

export function ConfigurationHtmlField({
  field,
  value,
  disabled,
  onChange,
}: {
  readonly field: AdminConfigField;
  readonly value: unknown;
  readonly disabled: boolean;
  readonly onChange: (key: string, next: unknown) => void;
}) {
  const text = typeof value === 'string' ? value : '';
  const preview = useMemo(() => sanitizeSplashHtml(text), [text]);
  // What the sanitiser removed. Comparing the two strings is a blunt signal and
  // that is what makes it honest: it says "this is not what you typed" without
  // claiming to explain which tag went, which would be a second forbid-list to
  // keep in step with the first.
  const changed = preview.trim() !== text.trim();

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
      <TextField
        size="small"
        label={field.title || field.key}
        helperText={field.description}
        multiline
        minRows={6}
        value={text}
        disabled={disabled}
        data-testid={`admin-config-html-${field.key}`}
        onChange={(event) => onChange(field.key, event.target.value)}
      />
      <Typography variant="bodySmall" color="text.secondary" component="div">
        {t('pages.admin.configuration.html.previewLabel', 'Preview')}
      </Typography>
      <Paper
        variant="outlined"
        data-testid={`admin-config-html-preview-${field.key}`}
        sx={{ padding: '1rem', minHeight: '4rem', overflowX: 'auto' }}
      >
        {text.trim() === '' ? (
          <Typography variant="bodySmall" color="text.secondary" component="div">
            {t(
              'pages.admin.configuration.html.previewEmpty',
              'Nothing to preview. Left empty, the platform shows its own splash screen.',
            )}
          </Typography>
        ) : (
          // Sanitised one line above, by the same function the live splash
          // uses. This is the only `dangerouslySetInnerHTML` on the admin page.
          <div dangerouslySetInnerHTML={{ __html: preview }} />
        )}
      </Paper>
      {changed && (
        <Typography
          variant="bodySmall"
          color="warning.main"
          component="div"
          data-testid={`admin-config-html-stripped-${field.key}`}
        >
          {t(
            'pages.admin.configuration.html.previewStripped',
            'The preview above is what users will see. Some markup was removed because it cannot run on the splash screen.',
          )}
        </Typography>
      )}
    </Box>
  );
}
