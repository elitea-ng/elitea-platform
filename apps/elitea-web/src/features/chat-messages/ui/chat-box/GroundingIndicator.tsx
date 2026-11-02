import { useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import type { Grounding } from '../../lib/grounding';
import { uncitedStatements } from '../../lib/grounding';

/**
 * A compact line under a citing agent's answer (ADR-0043 decision 5): how many
 * sources the answer cites, and how many statements have no citation. The
 * second part opens the list of those statements, each marked. Neutral
 * colours only: the counts are facts, not a confidence rating.
 */
export function GroundingIndicator({ grounding, answer }: { readonly grounding: Grounding; readonly answer: string }) {
  const [open, setOpen] = useState(false);
  const statements = open ? uncitedStatements(answer, grounding) : [];
  const more = grounding.uncitedSpans - grounding.uncited.length;
  return (
    <Box data-testid="grounding-indicator" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, alignSelf: 'stretch' }}>
      <Box sx={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', columnGap: 1 }}>
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.chatMessages.grounding.sourcesCited', '{{cited}} of {{available}} sources cited', {
            cited: grounding.sourcesCited, available: grounding.sourcesAvailable,
          })}
        </Typography>
        {grounding.uncitedSpans > 0 && (
          <Button
            size="small"
            variant="text"
            color="inherit"
            aria-expanded={open}
            onClick={() => setOpen((previous) => !previous)}
            sx={{ color: 'text.secondary', textTransform: 'none', minWidth: 0, px: 0.5 }}
          >
            {t('features.chatMessages.grounding.uncited', '{{count}} statements without a citation', { count: grounding.uncitedSpans })}
          </Button>
        )}
      </Box>
      {open && (
        <Box component="ul" data-testid="grounding-uncited-list" sx={{ m: 0, pl: 2.5 }}>
          {statements.map((statement, index) => (
            <Typography key={`${index}:${statement}`} component="li" variant="bodySmall">
              <Box component="mark" sx={{ bgcolor: 'action.selected', color: 'inherit', px: 0.25 }}>{statement}</Box>
            </Typography>
          ))}
          {more > 0 && (
            <Typography component="li" variant="bodySmall" color="text.secondary">
              {t('features.chatMessages.grounding.more', '{{count}} more not shown.', { count: more })}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  );
}
