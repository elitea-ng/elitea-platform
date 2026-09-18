import type { ReactNode } from 'react';

// `shared/ui/icons/` has no back-arrow glyph (only S2's 39 baseline-used
// icons were ported) — the same documented fallback
// `features/analytics/ui/components/DetailHeader.tsx` already uses for its
// own back button (R-I1 only bans BARREL imports, not this).
import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

/**
 * #897 — the agent editor's page header: a back arrow beside the agent's
 * name, distinct from the editable Name FIELD further down the form.
 *
 * Split out of `EditApplication.tsx` purely for the §3.4 400-line budget;
 * it owns no state and decides no navigation target — `onBack` is entirely
 * caller-owned, matching every other write control this page composes.
 */
export interface EditApplicationHeaderProps {
  readonly title: string;
  readonly onBack: () => void;
}

const titleSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };

export function EditApplicationHeader({ title, onBack }: EditApplicationHeaderProps): ReactNode {
  return (
    <Box
      sx={titleSx}
      data-testid="edit-application-header"
    >
      <IconButton
        size="small"
        onClick={onBack}
        aria-label={t('pages.agents.editApplication.back', 'Back')}
      >
        <ArrowBackIcon fontSize="small" />
      </IconButton>
      <Typography variant="headingSmall">{title}</Typography>
    </Box>
  );
}
