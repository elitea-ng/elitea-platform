import { Box, Typography } from '@mui/material';
import { memo, useEffect, useState } from 'react';

import MaintenanceTips from './MaintenanceTips';

import MaintenanceLogo from '@/entries/maintenance/assets/maintenance-logo.svg?react';
import ChatWelcomeImage from '@/entries/maintenance/assets/chat-welcome.png';
import { t } from '@/shared/i18n';
import { sanitizeSplashHtml } from '@/shared/ui/lib/sanitizeSplashHtml';

import { VITE_MAINTENANCE_START, VITE_MAINTENANCE_END, VITE_MAINTENANCE_MESSAGE } from '../constants';
import {
  fetchPublishedCopy,
  NO_PUBLISHED_COPY,
  type PublishedMaintenanceCopy,
} from '../lib/publishedCopy';

const maintenanceHeadline = t('entries.maintenance.page.headline', 'Elitea is under maintenance!');
const maintenanceLogoAlt = t('entries.maintenance.page.logoAlt', 'EliteA');

/**
 * The operator's copy, when the platform is up enough to publish it.
 *
 * Starts as nothing and stays nothing on any failure, so the first paint and
 * the offline case are both the page this entry has always rendered. See
 * `lib/publishedCopy.ts` for why this page asks at all.
 */
function usePublishedCopy(): PublishedMaintenanceCopy {
  const [copy, setCopy] = useState<PublishedMaintenanceCopy>(NO_PUBLISHED_COPY);
  useEffect(() => {
    let live = true;
    void fetchPublishedCopy().then((next) => {
      if (live) setCopy(next);
    });
    return () => {
      live = false;
    };
  }, []);
  return copy;
}

const MaintenancePage = memo(() => {
  const published = usePublishedCopy();
  return (
    <Box
      sx={(theme) => ({
        width: '100%',
        minWidth: '100%',
        height: '100vh',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        overflow: 'auto',
        background: theme.vars.palette.background.default,
        position: 'relative',
      })}
    >
      <Box
        sx={{
          width: '100%',
          maxWidth: '46.25rem',
          boxSizing: 'border-box',
          height: 'auto',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'flex-start',
          gap: '2rem',
          '@media (max-width: 900px)': {
            width: '34.5rem',
            height: 'auto',
          },
        }}
      >
        <Box
          sx={{
            width: '6.1875rem',
            height: '1.25rem',
          }}
        >
          <MaintenanceLogo />
        </Box>
        <Box
          sx={(theme) => ({
            height: 'auto',
            width: '100%',
            padding: '0.0625rem',
            borderRadius: theme.vars.shape.radiusLg,
            background: theme.vars.palette.background.onboarding,
            boxShadow: theme.vars.palette.boxShadow.onboarding,
            '@media (max-width: 900px)': {
              minHeight: '23.6875rem',
              height: 'auto',
            },
          })}
        >
          <Box
            sx={(theme) => ({
              width: '100%',
              minHeight: '100%',
              height: 'auto',
              background: theme.vars.palette.background.onboardingBody,
              borderRadius: `calc(${theme.vars.shape.radiusLg} - 0.0625rem)`,
              padding: '2rem',
              boxSizing: 'border-box',
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'center',
              gap: '2rem',
            })}
          >
            <Box
              component="img"
              sx={{ height: '3.75rem', width: '3.75rem' }}
              src={ChatWelcomeImage}
              alt={maintenanceLogoAlt}
            />
            <Box
              sx={{
                width: '100%',
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'center',
                gap: '0.5rem',
              }}
            >
              <Typography
                component="div"
                variant="headingLarge"
                sx={(theme) => ({ color: theme.vars.palette.text.secondary })}
              >
                {published.title || maintenanceHeadline}
              </Typography>
              {published.html !== '' && (
                <Box
                  data-testid="maintenance-published-html"
                  sx={(theme) => ({
                    width: '100%',
                    color: theme.vars.palette.text.secondary,
                    textAlign: 'center',
                    overflowX: 'auto',
                  })}
                  // Sanitised on this line, by the same function the admin
                  // editor previews through and the in-app splash renders
                  // through. The server also refuses executable markup on the
                  // way in; neither check makes the other redundant.
                  dangerouslySetInnerHTML={{ __html: sanitizeSplashHtml(published.html) }}
                />
              )}
              {published.html === '' && published.message !== '' && (
                <Typography
                  component="div"
                  data-testid="maintenance-published-message"
                  sx={(theme) => ({
                    width: '100%',
                    color: theme.vars.palette.text.secondary,
                    textAlign: 'center',
                  })}
                >
                  {published.message}
                </Typography>
              )}
              {VITE_MAINTENANCE_START && VITE_MAINTENANCE_END && (
                <>
                  <Typography
                    component="div"
                    sx={(theme) => ({
                      width: '100%',
                      color: theme.vars.palette.icon.fill.magicAssistant,
                      textAlign: 'center',
                    })}
                  >
                    {`From ${VITE_MAINTENANCE_START} to ${VITE_MAINTENANCE_END}`}
                  </Typography>
                  {VITE_MAINTENANCE_MESSAGE && (
                    <Typography
                      component="div"
                      sx={(theme) => ({
                        width: '100%',
                        color: theme.vars.palette.icon.fill.magicAssistant,
                        textAlign: 'center',
                      })}
                    >
                      {VITE_MAINTENANCE_MESSAGE}
                    </Typography>
                  )}
                </>
              )}
            </Box>
            <MaintenanceTips />
          </Box>
        </Box>
      </Box>
    </Box>
  );
});

MaintenancePage.displayName = 'MaintenancePage';

export default MaintenancePage;
