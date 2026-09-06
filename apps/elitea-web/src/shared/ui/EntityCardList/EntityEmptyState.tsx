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
 * The width every illustration is painted at — the baseline's `15rem`, stated
 * in pixels.
 *
 * WHY PIXELS, AND WHY A HEIGHT AT ALL (issue #819). The images were painted
 * `width: 15rem; height: auto`, so the box took its height from the source's
 * aspect ratio: 240 * 512 / 720 = 170.666…, which Blink lays out at
 * 170.65625px. Everything under it inherited that fraction — the heading at
 * `getBoundingClientRect().top = 361.40625`, the description at 401.40625,
 * device rows 722.8125 and 802.8125 at the visual suite's
 * `deviceScaleFactor: 2`. Layout was reproducible, but a 14px/400 glyph run
 * eight tenths of a device row off the grid is not guaranteed to rasterise the
 * same way twice, and it did not: the `pipelines-list-empty` baseline flipped
 * between two captures that differ ONLY over that text (1342 px, then
 * 2654 px), with nothing moved. The other illustrated pages sat on whole rows
 * by luck, not by construction.
 *
 * A whole-pixel painted box puts the heading, the description and the `+
 * Create` pill back on whole device rows. `rem` would only hold that at the
 * default 16px root; the illustration is decorative (`alt=""`), so pinning it
 * costs no text scaling and buys the guarantee at every root size.
 */
const ART_WIDTH_PX = 240;

interface EmptyStateArtVariant {
  /** The bundled WebP URL. */
  readonly src: string;
  /**
   * `ART_WIDTH_PX * sourceHeight / sourceWidth`, rounded to a whole pixel —
   * at most 0.34px away from the source ratio, which `object-fit: contain`
   * absorbs as a letterbox rather than as distortion.
   */
  readonly heightPx: number;
}

/**
 * The six sources are NOT one shape: the light exports are cropped shorter
 * than the dark ones. Each variant therefore carries its own rounded height;
 * light and dark already differed before this table existed, and matching them
 * would move the light empty states by 6px.
 *
 * `EntityCardList.test.tsx`'s "device grid" block re-reads the six WebP
 * headers and fails if a re-encode moves any of them away from these numbers.
 */
const EMPTY_STATE_ART = {
  applications: {
    /** 720x511 -> 170.33 */
    light: { src: applicationsLight, heightPx: 170 },
    /** 720x512 -> 170.67 */
    dark: { src: applicationsDark, heightPx: 171 },
  },
  skills: {
    /** 720x496 -> 165.33 */
    light: { src: skillsLight, heightPx: 165 },
    /** 720x512 -> 170.67 */
    dark: { src: skillsDark, heightPx: 171 },
  },
  credentials: {
    /** 720x496 -> 165.33 */
    light: { src: credentialsLight, heightPx: 165 },
    /** 720x512 -> 170.67 */
    dark: { src: credentialsDark, heightPx: 171 },
  },
} as const satisfies Record<string, { readonly light: EmptyStateArtVariant; readonly dark: EmptyStateArtVariant }>;

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

/**
 * The "you have nothing here yet" screen a list page shows in place of its
 * grid — ported from `apps/elitea-ui/src/[fsd]/entities/empty-state-page/ui/
 * EmptyStatePage.jsx`: a 240px illustration, an 18px/600 heading, a 14px
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
          src={images.light.src}
          alt=""
          width={ART_WIDTH_PX}
          height={images.light.heightPx}
          sx={lightImageSx(images.light.heightPx)}
        />
        <Box
          component="img"
          src={images.dark.src}
          alt=""
          width={ART_WIDTH_PX}
          height={images.dark.heightPx}
          sx={darkImageSx(images.dark.heightPx)}
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

/**
 * The painted box, stated in whole pixels in BOTH axes.
 *
 * The `width`/`height` attributes alone would not hold it: they only supply
 * `aspect-ratio: auto <w>/<h>`, and `auto` hands the ratio back to the source
 * bitmap the moment it decodes. They are still worth carrying — they reserve
 * this exact box before the WebP arrives, which is the 170px jump
 * `e2e/visual/lib/settle.ts` documents — but the CSS is what keeps the box
 * whole after the decode.
 */
const artBoxSx = (heightPx: number): Record<string, string> => ({
  width: `${String(ART_WIDTH_PX)}px`,
  height: `${String(heightPx)}px`,
  // The rounded height is up to 0.34px taller than the source ratio. `contain`
  // spends that on a letterbox band instead of stretching the artwork.
  objectFit: 'contain',
});

const lightImageSx =
  (heightPx: number): SxProps<Theme> =>
  (theme: Theme) => ({
    ...artBoxSx(heightPx),
    display: 'block',
    ...theme.applyStyles('dark', { display: 'none' }),
  });

const darkImageSx =
  (heightPx: number): SxProps<Theme> =>
  (theme: Theme) => ({
    ...artBoxSx(heightPx),
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
