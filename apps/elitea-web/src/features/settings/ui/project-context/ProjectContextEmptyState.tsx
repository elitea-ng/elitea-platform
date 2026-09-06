/**
 * ProjectContextEmptyState — what Settings › Project Context shows before a
 * project has any context saved.
 *
 * Ported from `EliteaUI/src/[fsd]/features/settings/ui/project-context/
 * ProjectContextEmptyState.jsx`, which this app had no counterpart for: it
 * went straight to the editor, so a project with nothing saved showed a bare
 * enable/disable switch and a disabled Save button, with no explanation of
 * what a project context is and no way to start one that reads as an
 * invitation. A live deployment shows this screen instead — a centred glyph,
 * a heading, one paragraph, and two buttons.
 *
 * The two buttons are the reference's `BUTTON_VARIANTS.elitea` and
 * `BUTTON_VARIANTS.special`, which this app already carries as the `elitea`
 * and `special` MuiButton variants. Measured on the live page they are the
 * filled cyan pill and the translucent-cyan pill with the sparkle.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import SvgIcon from '@mui/material/SvgIcon';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AiSparkleIcon } from '@/shared/ui/icons/ai-sparkle-icon';
import { ContextIcon } from '@/shared/ui/icons/context-icon';

export interface ProjectContextEmptyStateProps {
  /** `PERMISSIONS.projectContext.edit`. Without it the copy changes and the buttons go. */
  readonly canEdit: boolean;
  /** Open the editor on a blank context. */
  readonly onCreate: () => void;
  /** Open the editor AND the "generate with AI" dialog — the reference's `onNavigate('create', { openAi: true })`. */
  readonly onBuildWithAi: () => void;
}

export const ProjectContextEmptyState = memo(function ProjectContextEmptyState({
  canEdit,
  onCreate,
  onBuildWithAi,
}: ProjectContextEmptyStateProps) {
  return (
    <Box sx={bodySx} data-testid="project-context-empty-state">
      <SvgIcon component={ContextIcon} inheritViewBox sx={imageSx} />
      <Typography variant="headingSmall" sx={titleSx}>
        {t('entities.projectContext.empty.title', 'Still no Project Context')}
      </Typography>
      <Typography sx={descriptionSx}>
        {canEdit
          ? t(
              'entities.projectContext.empty.description',
              'Let’s create project-specific background information that the AI will use to generate more accurate and relevant responses, tailored to your workflows, data, and goals.',
            )
          : t(
              'entities.projectContext.empty.descriptionReadOnly',
              'Project Context has not been set up yet. Contact your project admin to configure it.',
            )}
      </Typography>
      {canEdit && (
        <Box sx={actionsSx}>
          <Button
            data-testid="project-context-create-button"
            variant="elitea"
            color="primary"
            onClick={onCreate}
          >
            {t('entities.projectContext.empty.create', 'Create')}
          </Button>
          <Button
            data-testid="project-context-build-with-ai-button"
            variant="special"
            startIcon={<SvgIcon component={AiSparkleIcon} inheritViewBox sx={sparkleSx} />}
            onClick={onBuildWithAi}
          >
            {t('entities.projectContext.empty.buildWithAi', 'Build with AI')}
          </Button>
        </Box>
      )}
    </Box>
  );
});

/** Reference `getStyles().body`. Measured live: gap 24px, padding-top 24px, centred. */
const bodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  flex: 1,
  gap: '1.5rem',
  padding: '1.5rem 0 0 0',
  textAlign: 'center',
  width: '100%',
};

const imageSx: SxProps<Theme> = { width: '2.5rem', height: '2.5rem' };

/*
 * `headingSmall` is 0.875rem/600; the reference overrides the SIZE to
 * 1.125rem while keeping the weight, and the live page confirms it
 * (`18px/600/24px`). The colour goes in `sx` because `color="text.secondary"`
 * emits no rule in this MUI setup.
 */
const titleSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  // oxlint-disable-next-line elitea/ad-hoc-font-size — reference override of the headingSmall step
  fontSize: '1.125rem',
  fontWeight: 600,
});

/* Live page: `14px/400`, line-height 21px (1.5), colour `rgb(202, 208, 216)`
 * — this app's `text.metrics` — capped at 480px (30rem). */
const descriptionSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.metrics,
  // oxlint-disable-next-line elitea/ad-hoc-font-size — ported from baseline
  fontSize: '0.875rem',
  lineHeight: 1.5,
  maxWidth: '30rem',
});

const actionsSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: '0.75rem',
  marginTop: '0.5rem',
};

const sparkleSx: SxProps<Theme> = { width: '1rem', height: '1rem' };
