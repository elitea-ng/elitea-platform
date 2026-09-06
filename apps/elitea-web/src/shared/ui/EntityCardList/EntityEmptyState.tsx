import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import applicationsDark from '@/assets/empty-states/applications-dark.webp';
import applicationsLight from '@/assets/empty-states/applications-light.webp';
import credentialsDark from '@/assets/empty-states/credentials-dark.webp';
import credentialsLight from '@/assets/empty-states/credentials-light.webp';
import skillsDark from '@/assets/empty-states/skills-dark.webp';
import skillsLight from '@/assets/empty-states/skills-light.webp';

/**
 * The "you have nothing here yet" screen a list page shows in place of its
 * grid — ported from `apps/elitea-ui/src/[fsd]/entities/empty-state-page/ui/
 * EmptyStatePage.jsx`: a 15rem illustration, an 18px/600 heading, a 14px
 * description capped at 24rem, and a `+ Create` pill.
 *
 * The three illustration pairs the baseline ships
 * (`assets/images/{Applications,Skills,Credentials}_{Dark,Light}*.png`,
 * 1446px wide, ~1.1MB each) are vendored here re-encoded as 720px WebP
 * (~100KB each) — they are only ever painted at 240px, so the original
 * resolution was 6MB of dead weight in the bundle.
 *
 * The dark/light pick is CSS, not a `palette.mode` branch in JS
 * (`elitea/no-mode-branch`, R-T2): both images render and
 * `theme.applyStyles('dark', …)` hides one.
 */
const EMPTY_STATE_ART = {
  applications: { dark: applicationsDark, light: applicationsLight },
  skills: { dark: skillsDark, light: skillsLight },
  credentials: { dark: credentialsDark, light: credentialsLight },
} as const;

/** @public Which of the three vendored illustration pairs an empty state paints. */
export type EmptyStateArt = keyof typeof EMPTY_STATE_ART;

export interface EntityEmptyStateProps {
  readonly title: string;
  readonly description: string;
  readonly art?: EmptyStateArt;
  readonly onCreateClick?: () => void;
  /** Overrides the default `Create` label (e.g. `Create credential`). */
  readonly createLabel?: string;
  /** `EmptyStatePage.jsx`'s second, secondary CTA — rendered under `Create` when supplied. */
  readonly onGuidedTourClick?: () => void;
}

export function EntityEmptyState({ title, description, art = 'applications', onCreateClick, createLabel, onGuidedTourClick }: EntityEmptyStateProps): ReactNode {
  const images = EMPTY_STATE_ART[art];
  return (
    <Box
      sx={containerSx}
      data-testid="entity-empty-state"
    >
      <Box sx={imageWrapperSx}>
        <Box
          component="img"
          src={images.light}
          alt=""
          sx={lightImageSx}
        />
        <Box
          component="img"
          src={images.dark}
          alt=""
          sx={darkImageSx}
        />
      </Box>
      <Typography
        variant="headingSmall"
        data-testid="empty-state-title"
        sx={titleSx}
      >
        {title}
      </Typography>
      <Typography
        variant="bodyMedium"
        sx={descriptionSx}
      >
        {description}
      </Typography>
      {(onCreateClick !== undefined || onGuidedTourClick !== undefined) && (
        <Box sx={actionsSx}>
          {onCreateClick !== undefined && (
            <BaseBtn
              variant="special"
              startIcon={<PlusIcon />}
              onClick={onCreateClick}
            >
              {createLabel ?? t('shared.entityList.emptyState.create', 'Create')}
            </BaseBtn>
          )}
          {onGuidedTourClick !== undefined && (
            <BaseBtn
              variant="secondary"
              onClick={onGuidedTourClick}
            >
              {t('shared.entityList.emptyState.guidedTour', 'Start Guided Tour')}
            </BaseBtn>
          )}
        </Box>
      )}
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  textAlign: 'center',
  gap: theme.spacing(2),
  paddingTop: theme.spacing(6),
  paddingBottom: theme.spacing(6),
  paddingLeft: theme.spacing(3),
  paddingRight: theme.spacing(3),
  width: '100%',
  height: '100%',
  minHeight: 0,
});

const imageWrapperSx: SxProps<Theme> = (theme: Theme) => ({ marginBottom: theme.spacing(2) });

const lightImageSx: SxProps<Theme> = (theme: Theme) => ({
  width: '15rem',
  height: 'auto',
  display: 'block',
  ...theme.applyStyles('dark', { display: 'none' }),
});

const darkImageSx: SxProps<Theme> = (theme: Theme) => ({
  width: '15rem',
  height: 'auto',
  display: 'none',
  ...theme.applyStyles('dark', { display: 'block' }),
});

const titleSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

const descriptionSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.background.tooltip.default,
  maxWidth: '28rem',
});

const actionsSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  marginTop: theme.spacing(1),
});
