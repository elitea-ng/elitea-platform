/**
 * ModelCapabilitiesSection — displays capability chips for a model.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/ai-configuration/Configuration/ModelCapabilitiesSection.jsx`.
 */
import { memo } from 'react';

import { useTheme } from '@mui/material/styles';
import type { Theme } from '@mui/material/styles';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/* Not exported: `ProjectAIConfiguration`'s sibling prop type is local too,
   and the page composes this component through `aiConfigurationFeature`. */
interface ModelCapabilitiesSectionProps {
  capabilities: readonly string[];
}

export default memo(function ModelCapabilitiesSection({ capabilities }: ModelCapabilitiesSectionProps) {
  const theme = useTheme();
  const styles = modelCapabilitiesSectionStyles(theme);

  if (!capabilities || capabilities.length === 0) {
    return null;
  }

  return (
    <Box sx={styles.capabilitiesSection}>
      {/* `component="h4"` is load-bearing, not decoration. The row sits after
          the configuration accordions, whose titles render at level 3, so a
          bare `variant="h6"` emitted an <h6> straight after an <h3> and axe's
          `heading-order` rule failed the whole AI-Configuration screen. The
          row only appears once a model with declared capabilities resolves,
          so the violation stayed hidden until the configurations runtime
          started serving one. `variant` keeps the type scale; `component`
          fixes the document outline. */}
      <Typography variant="h6" component="h4" sx={styles.sectionTitle}>
        {t('ai-configuration.modelCapabilities.title', 'Model Capabilities')}
      </Typography>
      <Box sx={styles.capabilitiesContainer}>
        {capabilities.map((capability, index) => (
          <Chip key={index} label={capability} size="small" sx={styles.capabilityChip} />
        ))}
      </Box>
    </Box>
  );
});

function modelCapabilitiesSectionStyles(theme: Theme) {
  return {
    capabilitiesSection: { flexShrink: 0 },
    sectionTitle: {
      color: theme.vars.palette.text.secondary,
      fontWeight: 600,
      fontSize: theme.typography.headingMedium.fontSize,
      display: 'flex',
      alignItems: 'center',
      gap: '0.5rem',
    },
    capabilitiesContainer: { display: 'flex', flexWrap: 'wrap', gap: '0.5rem' },
    capabilityChip: {
      backgroundColor: theme.vars.palette.primary.main,
      color: theme.vars.palette.common.white,
      fontWeight: 500,
      fontSize: theme.typography.bodySmall.fontSize,
      height: '1.75rem',
      borderRadius: theme.shape.radiusLg,
      border: 'none',
      '&:hover': {
        backgroundColor: theme.vars.palette.primary.dark,
      },
    },
  };
}
