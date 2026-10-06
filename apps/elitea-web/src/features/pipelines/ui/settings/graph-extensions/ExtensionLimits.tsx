import type { ReactNode } from 'react';
import Accordion from '@mui/material/Accordion';
import AccordionSummary from '@mui/material/AccordionSummary';
import AccordionDetails from '@mui/material/AccordionDetails';
import Stack from '@mui/material/Stack';
import { t } from '@/shared/i18n';
import { extensionNumberText, extensionRecord, patchExtensionRecord } from '../../../lib/graphExtensions.helpers';
import { SHAPING_LIMITS, type ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { ExtensionField } from './ExtensionFields';

export function ExtensionLimits({ node, disabled, change, remove }: ExtensionSettingsProps): ReactNode {
  const limits = extensionRecord(node['limits']);
  return <Accordion className="nodrag nopan nowheel">
    <AccordionSummary>{t('pipelines.graphExtensions.limits', 'Processing limits')}</AccordionSummary>
    <AccordionDetails><Stack spacing={2}>
      {Object.entries(SHAPING_LIMITS).map(([key, maximum]) => <ExtensionField key={key}
        label={t(`pipelines.graphExtensions.limits.${key}`, key)} value={extensionNumberText(limits[key])} disabled={disabled} number
        help={t('pipelines.graphExtensions.limitCeiling', 'Leave empty for {{maximum}}. A configured limit can only lower this ceiling.', { maximum })}
        change={(value) => {
          const next = patchExtensionRecord(limits, key, Number(value), value === '');
          if (Object.keys(next).length === 0) remove('limits');
          else change('limits', next);
        }} />)}
    </Stack></AccordionDetails>
  </Accordion>;
}
