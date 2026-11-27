/**
 * The Memory tab's settings (issue 3137, ADR-0050 decision 1b): whether the
 * agent remembers, how its writes are approved, and how much it recalls each
 * turn — `meta.memory`, saved with the version.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { MetaNumberField, MetaSelectField, MetaSwitchField, useMetaBlock, type MetaSectionProps } from './metaControls';
import { metaFieldsSx, metaSectionSx } from './metaSection.styles';

export function MemorySettingsSection({ versionFields, disabled }: MetaSectionProps): ReactNode {
  const memory = useMetaBlock(versionFields, 'memory');
  return (
    <Box component="section" sx={metaSectionSx} data-testid="agent-meta-memory" aria-labelledby="agent-meta-memory-title">
      <Typography id="agent-meta-memory-title" component="h3" variant="headingSmall">
        {t('pages.agents.editApplication.meta.memory.title', 'Memory settings')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('pages.agents.editApplication.meta.memory.intro', 'Saved with the version. Without settings the agent has no memory.')}
      </Typography>
      <MetaSwitchField handle={memory} path={['enabled']} keepFalse label={t('pages.agents.editApplication.meta.memory.enabled', 'Remember across conversations')} disabled={disabled} testId="agent-meta-memory-enabled" />
      <Box sx={metaFieldsSx}>
        <MetaSelectField
          handle={memory}
          path={['write']}
          label={t('pages.agents.editApplication.meta.memory.write', 'New memories')}
          helper={t('pages.agents.editApplication.meta.memory.writeHelper', 'How a memory the agent writes becomes active.')}
          defaultLabel={t('pages.agents.editApplication.meta.default', 'Default')}
          options={[
            { value: 'auto_attended', label: t('pages.agents.editApplication.meta.memory.autoAttended', 'Active when a person started the turn, else for review') },
            { value: 'auto', label: t('pages.agents.editApplication.meta.memory.auto', 'Always active') },
            { value: 'review', label: t('pages.agents.editApplication.meta.memory.review', 'Always for review') },
            { value: 'off', label: t('pages.agents.editApplication.meta.memory.off', 'Off: recall only') },
          ]}
          disabled={disabled}
          testId="agent-meta-memory-write"
        />
        <MetaNumberField handle={memory} path={['recall', 'max_entries']} label={t('pages.agents.editApplication.meta.memory.maxEntries', 'Memories recalled per turn')} helper={t('pages.agents.editApplication.meta.range', 'From {{min}} to {{max}}.', { min: 1, max: 50 })} disabled={disabled} testId="agent-meta-memory-max-entries" />
        <MetaNumberField handle={memory} path={['recall', 'max_tokens']} label={t('pages.agents.editApplication.meta.memory.maxTokens', 'Recall budget (tokens)')} helper={t('pages.agents.editApplication.meta.range', 'From {{min}} to {{max}}.', { min: 100, max: 8000 })} disabled={disabled} testId="agent-meta-memory-max-tokens" />
      </Box>
    </Box>
  );
}
