/**
 * What a listing version needs from the project (ADR-0051 decision 2): the
 * toolkit and MCP types, skills and nested agents, the model class and the
 * risk tier. Types and names only; the listing never carries a credential.
 * `compact` is the one-line form a version row uses.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';

import type { MarketplaceRequirements } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export function ListingRequirements({
  requirements,
  compact = false,
}: {
  readonly requirements: MarketplaceRequirements;
  readonly compact?: boolean;
}): ReactNode {
  const summary = t('marketplace.requirements.summary', 'Risk tier: {{tier}}. Model: {{model}}.{{writes}}', {
    tier: requirements.tier,
    model:
      requirements.model_class === 'tool_calling'
        ? t('marketplace.requirements.toolCalling', 'tool calling')
        : t('marketplace.requirements.chat', 'chat'),
    writes: requirements.has_write_tools ? ` ${t('marketplace.requirements.writes', 'Has tools that can write.')}` : '',
  });
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="marketplace-requirements">
      <Typography variant="bodySmall" color="text.secondary">
        {summary}
      </Typography>
      {requirements.items.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.requirements.none', 'It needs nothing from your project.')}
        </Typography>
      ) : (
        <Box
          component="ul"
          sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5, listStyle: 'none', margin: 0, padding: 0 }}
        >
          {requirements.items.map((item) => (
            <li key={item.key}>
              <Chip
                size="small"
                variant="outlined"
                label={
                  compact || item.tools.length === 0
                    ? `${item.kind}: ${item.type}`
                    : `${item.kind}: ${item.type} (${item.tools.join(', ')})`
                }
              />
            </li>
          ))}
        </Box>
      )}
    </Box>
  );
}
