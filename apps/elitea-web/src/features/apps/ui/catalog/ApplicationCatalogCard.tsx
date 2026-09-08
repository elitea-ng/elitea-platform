import { useCallback } from 'react';
import type { MouseEvent } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ClockIcon } from '@/shared/ui/icons/clock-icon';
import { GearIcon } from '@/shared/ui/icons/gear-icon';
import { LinkIcon } from '@/shared/ui/icons/link-icon';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { REQUEST_STATUS } from '../../lib/constants';
import type { RequestStatus } from '../../lib/constants';
import { cardGradientSx } from '@/shared/lib/cardGradient';
import type { CatalogApplication } from '../../model/types';

function lineClamp(lines: number) {
  return {
    width: '100%',
    wordWrap: 'break-word' as const,
    overflowWrap: 'break-word' as const,
    textOverflow: 'ellipsis' as const,
    overflow: 'hidden' as const,
    display: '-webkit-box' as const,
    WebkitBoxOrient: 'vertical' as const,
    WebkitLineClamp: String(lines),
  };
}

const headerSx = {
  display: 'flex',
  alignItems: 'center',
  gap: '1rem',
  marginBottom: '0.75rem',
};

const iconWrapperSx = {
  width: '2.25rem',
  height: '2.25rem',
  flexShrink: 0,
  display: 'flex',
};

const titleSx = {
  maxHeight: '3rem',
  ...lineClamp(2),
};

const contentSx = {
  flex: 1,
  display: 'flex',
  flexDirection: 'column' as const,
  justifyContent: 'space-between',
};

const descriptionContainerSx = {
  display: 'flex',
  flexDirection: 'column' as const,
  gap: '0.5rem',
  marginBottom: '1rem',
};

function descriptionTextSx(theme: Theme) {
  return {
    color: theme.vars.palette.text.secondary,
    ...lineClamp(3),
  };
}

const actionsSx = {
  display: 'flex',
  justifyContent: 'space-between',
  flexWrap: 'wrap' as const,
  alignItems: 'center',
  gap: '0.5rem',
  minHeight: '2rem',
};

const primaryActionsSx = {
  display: 'flex',
  alignItems: 'center',
  flexWrap: 'wrap' as const,
  gap: '0.5rem',
  minHeight: '1.75rem',
};

function configureButtonSx(theme: Theme) {
  return {
    '& svg': { width: '1rem', height: '1rem' },
    // `icon.fill.white` (the baseline's `palette.icon.fill.white`,
    // ApplicationCatalogCard.jsx:277) has no matching token in T1's brand
    // pack — `icon.fill.button` is `#FFFFFF` in BOTH schemes
    // (`default.pack.json`), i.e. the same scheme-independent white, and
    // is T1's designated token for an icon rendered on a filled/coloured
    // button (exactly this call site's context).
    '& svg path': { fill: theme.vars.palette.icon.fill.button },
  };
}

function pendingStatusSx(theme: Theme) {
  return {
    display: 'flex',
    alignItems: 'center',
    gap: '0.25rem',
    color: theme.vars.palette.status.onModeration,
  };
}

function documentationLinkSx(theme: Theme) {
  return {
    display: 'flex',
    alignItems: 'center',
    gap: '0.25rem',
    textDecoration: 'none',
    color: theme.vars.palette.text.default,
    transition: 'color 0.2s',
    '&:hover': { color: theme.vars.palette.text.secondary },
    '& svg': { width: '0.875rem', height: '0.875rem' },
  };
}

const documentationTextSx = {
  textDecoration: 'underline',
};

export interface ApplicationCatalogCardProps {
  application: CatalogApplication;
  requestStatus: RequestStatus;
  isLoading: boolean;
  isFetchingStatus: boolean;
  onConfigure: (application: CatalogApplication) => void;
  onRequestAccess: (application: CatalogApplication) => void;
}

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/apps/ui/catalog/ApplicationCatalogCard.jsx`.
 *
 * Dropped from the baseline: the `EntityIcon`/`HighlightQuery` wrapper
 * components — `EntityIcon`'s only role here was rendering a pre-built
 * gradient-ring icon slot around `application.icon_meta.component` (this
 * port renders `application.Icon` directly, no ring/editable affordance
 * needed for a static, non-editable catalogue entry); `HighlightQuery`
 * highlighted a global search-store keyword inside the title, but
 * `ApplicationCatalog.jsx` never threads a query into it (`state.search`,
 * a Redux slice this architecture does not carry forward per §2.3) — every
 * baseline call site rendered plain, always-unhighlighted text in
 * practice. `application.typeLabel`/`.description`/`.statusLabel` (used by
 * the baseline's OWN `tags`/`author` fields) are also dropped: reading the
 * baseline's card JSX directly, none of those three fields is ever
 * rendered by IT (they were built for the generic multi-purpose entity
 * card the hook's shape was borrowed from, `ToolkitsList`'s card, and are
 * genuinely dead in `ApplicationCatalogCard` itself — `typeLabel` IS used,
 * one level up, by `ApplicationCatalog.jsx`'s submit handler; ported there
 * instead, see that file).
 */
export function ApplicationCatalogCard({
  application,
  requestStatus,
  isLoading,
  isFetchingStatus,
  onConfigure,
  onRequestAccess,
}: ApplicationCatalogCardProps) {
  const Icon = application.Icon;
  const isPending = requestStatus === REQUEST_STATUS.PENDING;
  const canRequest = application.canRequest && !isPending;

  const handleConfigureClick = useCallback(
    (event: MouseEvent<HTMLButtonElement>) => {
      event.stopPropagation();
      onConfigure(application);
    },
    [application, onConfigure],
  );

  const handleRequestClick = useCallback(
    (event: MouseEvent<HTMLButtonElement>) => {
      event.stopPropagation();
      onRequestAccess(application);
    },
    [application, onRequestAccess],
  );

  const handleDocumentationClick = useCallback((event: MouseEvent<HTMLAnchorElement>) => {
    event.stopPropagation();
  }, []);

  return (
    <Box
      component="article"
      sx={(theme: Theme) => ({
        ...cardGradientSx(theme),
        padding: '1rem 1.25rem',
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
      })}
    >
      <Box sx={headerSx}>
        <Box sx={iconWrapperSx}>
          <Icon
            width="2.25rem"
            height="2.25rem"
          />
        </Box>
        <Tooltip
          placement="top"
          enterDelay={1000}
          enterNextDelay={1000}
          title={
            <>
              <Typography
                variant="bodySmall2"
                sx={{ fontWeight: 700, ...lineClamp(2) }}
              >
                {application.name}
              </Typography>
              <Typography
                variant="bodySmall2"
                sx={lineClamp(4)}
              >
                {application.description}
              </Typography>
            </>
          }
        >
          <Typography
            color="text.secondary"
            variant="headingSmall"
            sx={titleSx}
          >
            {application.name}
          </Typography>
        </Tooltip>
      </Box>

      <Box sx={contentSx}>
        <Box sx={descriptionContainerSx}>
          <Typography
            variant="bodySmall"
            sx={descriptionTextSx}
          >
            <Box component="b">{t('apps.catalogCard.useItTo', 'Use it to: ')}</Box>
            {application.shortDescription}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={descriptionTextSx}
          >
            <Box component="b">{t('apps.catalogCard.includes', 'Includes: ')}</Box>
            {application.capabilities.join(', ')}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={descriptionTextSx}
          >
            <Box component="b">{t('apps.catalogCard.bestFor', 'Best for: ')}</Box>
            {application.bestFor}
          </Typography>
        </Box>

        <Box sx={actionsSx}>
          <Box sx={primaryActionsSx}>
            {isFetchingStatus ? (
              <CircularProgress size={18} />
            ) : (
              <>
                {application.canCreate && !isPending && (
                  <BaseBtn
                    variant="special"
                    startIcon={<GearIcon />}
                    disabled={isLoading}
                    sx={configureButtonSx}
                    onClick={handleConfigureClick}
                  >
                    <Typography
                      component="span"
                      variant="labelSmall"
                    >
                      {t('apps.catalogCard.configure', 'Configure')}
                    </Typography>
                  </BaseBtn>
                )}

                {canRequest && (
                  <BaseBtn
                    variant="auxiliary"
                    onClick={handleRequestClick}
                  >
                    <Typography
                      component="span"
                      variant="labelSmall"
                    >
                      {t('apps.catalogCard.requestAccess', 'Request Access')}
                    </Typography>
                  </BaseBtn>
                )}

                {isPending && (
                  <Box sx={pendingStatusSx}>
                    <ClockIcon />
                    <Typography variant="labelSmall">{t('apps.catalogCard.pendingApproval', 'Pending approval')}</Typography>
                  </Box>
                )}
              </>
            )}
          </Box>

          <Box
            component="a"
            href={application.documentation}
            target="_blank"
            rel="noopener noreferrer"
            onClick={handleDocumentationClick}
            sx={documentationLinkSx}
          >
            <Typography
              component="span"
              variant="labelSmall"
              sx={documentationTextSx}
            >
              {t('apps.catalogCard.documentation', 'Documentation')}
            </Typography>
            <LinkIcon />
          </Box>
        </Box>
      </Box>
    </Box>
  );
}
