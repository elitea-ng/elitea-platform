/**
 * The full-page 403 `AdminApp.tsx` renders in place of the admin router for a
 * caller `hasAnyAdminNavAccess()` refuses (smoke finding: a non-admin who
 * opened `/admin/app` saw the whole console shell — every write still came
 * back 403 from the server, but the page itself never said so).
 *
 * No `AdminLayout`, no `AdminNav`, no `Outlet` — this is what mounts INSTEAD
 * of the route tree, not a route inside it, so a stale bookmark or a typed
 * URL for any of the eleven pages lands here too.
 *
 * The page sends the caller back to the app after `REDIRECT_SECONDS`, with a
 * live countdown and a "Stay on this page" control (WCAG 2.2.1: the timing is
 * adjustable). The SPA is served 200 by the Go handler — the "403" is the
 * client-side gate, see `admin.access-denied.spec.ts`.
 */
import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { StatusPage } from '@/shared/ui/status-page';

export const REDIRECT_SECONDS = 5;
const APP_URL = '/app/';

export interface AdminAccessDeniedProps {
  /** Navigation seam so tests do not touch `window.location`. */
  readonly redirect?: (url: string) => void;
}

function assignLocation(url: string): void {
  window.location.assign(url);
}

function ShieldIcon(): ReactNode {
  return (
    <svg
      width="56"
      height="56"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
    >
      <path d="M12 3 4.5 6v5.5c0 4.4 3 8.2 7.5 9.5 4.5-1.3 7.5-5.1 7.5-9.5V6L12 3Z" />
      <path d="m9.5 9.5 5 5m0-5-5 5" />
    </svg>
  );
}

export function AdminAccessDenied({ redirect = assignLocation }: AdminAccessDeniedProps = {}) {
  // `null` once the visitor chose to stay: the timer stops and the countdown goes away.
  const [remaining, setRemaining] = useState<number | null>(REDIRECT_SECONDS);
  const homeLinkRef = useRef<HTMLAnchorElement>(null);

  useEffect(() => {
    if (remaining === null) return undefined;
    if (remaining <= 0) {
      redirect(APP_URL);
      return undefined;
    }
    const id = window.setTimeout(() => {
      setRemaining(remaining - 1);
    }, 1000);
    return () => {
      window.clearTimeout(id);
    };
  }, [remaining, redirect]);

  return (
    <StatusPage
      testId="admin-access-denied"
      icon={<ShieldIcon />}
      title={t('pages.admin.accessDenied.title', 'Nice Try, Hacker!')}
      actions={
        <>
          <Button variant="contained" component="a" href={APP_URL} ref={homeLinkRef}>
            {t('pages.admin.accessDenied.backToApp', 'Back to the app now')}
          </Button>
          {remaining !== null ? (
            <Button
              variant="outlined"
              onClick={() => {
                // The button unmounts on cancel; hand focus to the primary link so
                // keyboard and screen-reader users keep their place (WCAG 2.4.3).
                setRemaining(null);
                homeLinkRef.current?.focus();
              }}
            >
              {t('pages.admin.accessDenied.stay', 'Stay on this page')}
            </Button>
          ) : null}
        </>
      }
    >
      <Typography
        component="p"
        variant="bodyMedium"
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 })}
      >
        {t(
          'pages.admin.accessDenied.explanation',
          'Your account does not hold an administration permission for this console.',
        )}
      </Typography>
      <Box
        component="output"
        aria-live="polite"
        data-testid="admin-access-denied-countdown"
        sx={(theme: Theme) => ({
          color: theme.vars.palette.text.secondary,
          ...theme.typography.bodySmall,
          display: 'block',
          margin: 0,
          minHeight: '1.25rem',
        })}
      >
        {remaining === null
          ? t('pages.admin.accessDenied.stayed', 'Automatic redirect cancelled.')
          : t('pages.admin.accessDenied.countdown', 'Taking you back to the app in {{seconds}} s…', {
              seconds: Math.max(remaining, 0),
            })}
      </Box>
    </StatusPage>
  );
}
