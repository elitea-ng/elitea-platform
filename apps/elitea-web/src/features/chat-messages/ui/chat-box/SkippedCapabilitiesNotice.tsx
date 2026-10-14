import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import type { SkippedCapabilities, SkippedCapability } from '../../lib/skippedCapabilities';

function label(item: SkippedCapability): string {
  if (item.kind === 'internal_tool') return t('features.chatMessages.skippedCapabilities.internalTool', 'Built-in capability {{name}}', { name: item.name });
  if (item.kind === 'application') return t('features.chatMessages.skippedCapabilities.application', 'Attached agent {{name}}', { name: item.name });
  return item.type
    ? t('features.chatMessages.skippedCapabilities.toolkitTyped', 'Toolkit {{name}} ({{type}})', { name: item.name, type: item.type })
    : t('features.chatMessages.skippedCapabilities.toolkit', 'Toolkit {{name}}', { name: item.name });
}

/** One compact notice per reply naming each capability the agent could not use, and why. */
export function SkippedCapabilitiesNotice({ skipped }: { readonly skipped: SkippedCapabilities }) {
  const { items, more } = skipped;
  if (items.length === 0) return null;
  return (
    <Alert severity="info" data-testid="skipped-capabilities-notice" sx={{ alignSelf: 'stretch', width: '100%' }}>
      <AlertTitle>{t('features.chatMessages.skippedCapabilities.title', 'Some tools were not available in this reply')}</AlertTitle>
      {items.map((item) => (
        <Typography key={`${item.kind}:${item.name}:${item.type ?? ''}:${item.reasonCode}`} variant="bodySmall" component="p">
          {t('features.chatMessages.skippedCapabilities.entry', '{{label}}: {{reason}}.', {
            label: label(item), reason: item.reason.replace(/\.$/, ''),
          })}
        </Typography>
      ))}
      {more > 0 && (
        <Typography variant="bodySmall" component="p">
          {t('features.chatMessages.skippedCapabilities.more', '{{count}} more not shown.', { count: more })}
        </Typography>
      )}
    </Alert>
  );
}
