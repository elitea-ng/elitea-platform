/**
 * EnableToggleCard — toggle card for enabling/disabling project context.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/project-context/EnableToggleCard.jsx`.
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { t } from '@/shared/i18n';

export interface EnableToggleCardProps {
  enabled: boolean;
  onToggle: (checked: boolean) => void;
  disabled?: boolean;
  /**
   * Heading and body copy. Default to Project Context's, which is the only
   * caller this card had; Settings › General's "Agent & Pipeline Builder"
   * reuses the same card with its own words, exactly as the reference does
   * (`project-general/AgentPipelineBuilder.jsx` passes `title`/`description`
   * into this component).
   */
  title?: string;
  description?: string;
}

export function EnableToggleCard({
  enabled,
  onToggle,
  disabled = false,
  title,
  description,
}: EnableToggleCardProps) {
  const sx = cardStyles();
  return (
    <Box sx={sx.card}>
      <Box sx={sx.text}>
        <Typography
          variant="headingSmall"
          color="text.secondary"
        >
          {title ?? t('entities.projectContext.enableToggleCard.title', 'Project Context')}
        </Typography>
        <Typography variant="bodySmall">
          {description ??
            t(
              'entities.projectContext.enableToggleCard.description',
              'Project-specific background information that the AI uses to generate more accurate and relevant responses, tailored to your workflows, data, and goals.',
            )}
        </Typography>
      </Box>
      <BaseSwitch
        checked={enabled}
        onChange={(e: React.ChangeEvent<HTMLInputElement>) => onToggle(e.target.checked)}
        color="primary"
        disabled={disabled}
      />
    </Box>
  );
}

function cardStyles(): Record<string, SxProps<Theme>> {
  return {
    card: (theme) => ({
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'space-between',
      padding: '1rem 1.5rem',
      borderRadius: 'var(--el-shape-radiusMd, 8px)',
      backgroundColor: theme.vars.palette.background.userInputBackground,
      gap: '1rem',
    }),
    text: {
      display: 'flex',
      flexDirection: 'column',
      gap: '0.25rem',
    },
  };
}
