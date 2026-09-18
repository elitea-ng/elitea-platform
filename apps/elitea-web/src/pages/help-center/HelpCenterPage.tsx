/**
 * HelpCenterPage — main page component.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/resources/index.jsx`.
 *
 * Admin-configured data (per-card enabled flags + links, version label,
 * plugin list) comes from `useResourcesConfig()` — see that hook's module
 * doc for the backend API gap (issue #26 Key Decision #2) that currently
 * keeps every card's links and the version bar empty.
 */
import { memo, useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import type { SxProps, Theme } from '@mui/material/styles';
import type { SystemStyleObject } from '@mui/system';
import Typography from '@mui/material/Typography';

import { RESOURCES_TOUR_TARGET_IDS } from '@/features/interactive-tours';
import { t } from '@/shared/i18n';

import { RESOURCE_CARD_CONFIGS } from './lib/ResourceCardConfig';
import { useResourcesConfig } from './lib/useResourcesConfig';
import { ResourceCard } from './ui/ResourceCard';
import ResourceVersionInfo from './ui/ResourceVersionInfo';

/**
 * Per-card link shape used when the admin config API is available.
 * Mirrors the shape consumed by the old `resources/index.jsx`. `badge` is
 * the one extra field `defaultLinks` (`ResourceCardConfig.ts`) can carry —
 * an admin-authored link never sets it, but the type is shared so a link
 * from either source renders through the same JSX branch (#891).
 */
export interface ResourceLink {
  title: string;
  url?: string;
  badge?: string;
}

/**
 * Resolves a card's links. A NON-EMPTY admin-configured value in
 * `configValues` (`configValues[config.linksKey]`) always wins; with none,
 * falls back to the card's own `defaultLinks` (#891/ELITEA-0967,0972,0973,
 * 0975 — a fresh deployment showed "No links configured" on every card
 * because no card shipped a default) — `[]` only when the card declares no
 * `defaultLinks` at all (Video Library: no real content exists to default
 * to, see that config entry's own comment).
 *
 * An EMPTY array counts as "not configured", not as "admin explicitly wants
 * zero links": `GET /admin/plugin_config_values/prompt_lib/resources`
 * answers every card's `linksKey` as `[]` on a fresh deployment (confirmed
 * live) — this section carries no separate "touched vs. never touched"
 * signal, so an empty array IS what "unconfigured" looks like on the wire,
 * exactly the case `useResourcesConfig.ts`'s own header describes.
 *
 * Exported so its parsing logic can be regression-tested independently of
 * whether a real config source is wired up yet (see `useResourcesConfig`).
 */
export function resolveLinks(
  config: (typeof RESOURCE_CARD_CONFIGS)[number],
  configValues: Record<string, unknown>,
): ReadonlyArray<ResourceLink> {
  const raw = configValues[config.linksKey];
  if (Array.isArray(raw) && raw.length > 0) return raw as ReadonlyArray<ResourceLink>;
  return config.defaultLinks ?? [];
}

/** Theme-aware sx values — MUI calls these functions with the theme. */
const pageSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  width: '100%',
  height: '100vh',
  display: 'flex',
  flexDirection: 'column',
  overflow: 'hidden',
  backgroundColor: t.vars.palette.background?.tabPanel ?? t.palette.background.default,
});

const contentSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  flex: 1,
  overflowY: 'auto',
  px: t.spacing(3),
  py: t.spacing(2),
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'stretch',
  gap: t.spacing(2),
  // #890 — axe `scrollable-region-focusable`: this Box scrolls its own
  // content (`overflowY: auto`) but had no `tabIndex` and no focusable
  // descendant a keyboard-only user could use to reach it, so it could
  // never be scrolled without a mouse/trackpad. `tabIndex={0}` (below, on
  // the element itself) plus an explicit `outline` on focus is the same
  // fix axe's own rule doc recommends for a scrolling container with no
  // native affordance of its own.
  // Same focus-ring shape `EntityCard.tsx` already uses for its own
  // keyboard-focusable, otherwise chrome-less container.
  '&:focus-visible': {
    outline: `0.125rem solid ${t.vars.palette.border.lines}`,
    outlineOffset: '-0.125rem',
  },
});

const introSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: '0.5rem',
  textAlign: 'center',
  py: '1rem',
  color: t.palette.text.secondary,
  width: '100%',
});

const gridSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(23.75rem, 31.25rem))',
  gap: '1rem',
  justifyContent: 'center',
};

const linkSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  color: t.palette.text?.metrics ?? t.palette.text.secondary,
  cursor: 'pointer',
  alignSelf: 'flex-start',
  textDecorationColor: 'currentColor',
  '&:hover': {
    color: t.palette.primary.main,
  },
});

const linkUndefinedSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  color: t.palette.text.disabled,
  display: 'block',
  fontStyle: 'italic',
});

/** Row wrapper so a badged link (#891) can sit beside its badge without disturbing the plain (unbadged) link's own layout. */
const linkRowSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  display: 'flex',
  alignItems: 'center',
  gap: t.spacing(1),
});

/** "Latest" pill — reuses the app's own chip tokens (`palette.suggestionChip`), not an invented color. */
const badgeSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  color: t.vars.palette.suggestionChip.text.default,
  backgroundColor: t.vars.palette.suggestionChip.background.default,
  border: `0.0625rem solid ${t.vars.palette.suggestionChip.border}`,
  borderRadius: t.vars.shape.radiusPill,
  padding: t.spacing(0.125, 0.75),
  lineHeight: 1.5,
  flexShrink: 0,
});

/**
 * Main HelpCenter page — renders the version header, intro, and
 * a responsive grid of ResourceCard components.
 */
const HelpCenterPage = memo((): ReactNode => {
  const { configValues, versionLabel, plugins } = useResourcesConfig();

  const visibleCards = useMemo(
    () =>
      RESOURCE_CARD_CONFIGS.filter(
        (config): config is (typeof RESOURCE_CARD_CONFIGS)[number] => configValues[config.enabledKey] !== false,
      ),
    [configValues],
  );

  return (
    <Box
      data-tour={RESOURCES_TOUR_TARGET_IDS.page}
      sx={pageSx}
    >
      <ResourceVersionInfo
        versionLabel={versionLabel}
        plugins={plugins}
      />

      <Box
        component="section"
        sx={contentSx}
        tabIndex={0}
        aria-label={t('pages.helpCenter.contentRegion', 'Help Center content')}
      >
        <Box sx={introSx}>
          <Typography variant="headingLarge">Explore Help Center</Typography>
          <Typography variant="bodyMedium">
            Guides, documentation, and release notes to support your work.
          </Typography>
        </Box>
        <Box sx={gridSx}>
          {visibleCards.map(config => {
            const links = resolveLinks(config, configValues);
            const hasLinks = links.length > 0;

            return (
              <ResourceCard
                key={config.enabledKey}
                title={config.defaultTitle}
                description={config.defaultDescription}
                colorScheme={config.colorScheme}
                tourTargetId={config.tourTargetId}
                icon={
                  <config.Icon
                    width="1.5rem"
                    height="1.5rem"
                  />
                }
              >
                {hasLinks &&
                  links.map((link, idx) =>
                    link.url ? (
                      <Box
                        key={idx}
                        sx={linkRowSx}
                      >
                        <Link
                          href={link.url}
                          target="_blank"
                          rel="noopener noreferrer"
                          underline="always"
                          sx={linkSx}
                          variant="bodyMedium"
                        >
                          {link.title}
                        </Link>
                        {/* #891/ELITEA-0975 — the Release Notes card's "Latest" badge
                         * over historical entries. The only card that sets `badge`
                         * today (see `ResourceCardConfig.ts`'s Release Notes entry). */}
                        {link.badge !== undefined && (
                          <Typography
                            variant="bodySmall"
                            sx={badgeSx}
                          >
                            {link.badge}
                          </Typography>
                        )}
                      </Box>
                    ) : (
                      <Typography
                        key={idx}
                        variant="bodyMedium"
                        sx={linkUndefinedSx}
                      >
                        {link.title} (undefined)
                      </Typography>
                    ),
                  )}
                {!hasLinks && (
                  <Typography
                    variant="bodySmall"
                    color="text.disabled"
                  >
                    No links configured
                  </Typography>
                )}
              </ResourceCard>
            );
          })}
        </Box>
      </Box>
    </Box>
  );
});

HelpCenterPage.displayName = 'HelpCenterPage';

export default HelpCenterPage;
