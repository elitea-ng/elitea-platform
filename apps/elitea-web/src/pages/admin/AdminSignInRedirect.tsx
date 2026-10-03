/**
 * Shown for `admin_ui_config.access === 'unauthenticated'`: the handler found
 * no usable session (missing, expired, revoked). That is a sign-in problem, not
 * a permission one, so the browser goes straight to the sign-in page and comes
 * back to the exact admin URL it asked for — no "access denied" copy.
 *
 * The return target is the current path + query, handed to `/auth/login` as
 * `target_to`, the parameter the chooser reads (browserauth/chooser.go). The
 * server canonicalises it and accepts only a same-origin absolute path; the
 * client additionally refuses anything that does not start with a single `/`.
 *
 * Loop guard: if the sign-in works but the session still does not reach this
 * page, redirecting again would spin forever. The return target carries an
 * `auth_retry=1` marker; a page that already has it shows a manual sign-in link
 * instead of redirecting a second time. (Stateless on purpose: raw web storage
 * is off-limits here, STOR-1.)
 */
import { useEffect } from 'react';
import type { ReactNode } from 'react';

import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';
import { StatusPage } from '@/shared/ui/status-page';

const LOGIN_PATH = '/auth/login';
const RETRY_PARAM = 'auth_retry';

export interface AdminSignInRedirectProps {
  /** Navigation seam so tests do not touch `window.location`. */
  readonly redirect?: (url: string) => void;
}

function replaceLocation(url: string): void {
  window.location.replace(url);
}

/** `/path?query` of the current page, or `/` when it is not a plain absolute path. */
function currentReturnTarget(location: Pick<Location, 'pathname' | 'search'>): string {
  const params = new URLSearchParams(location.search);
  params.set(RETRY_PARAM, '1');
  const path = location.pathname.startsWith('/') && !location.pathname.startsWith('//') ? location.pathname : '/';
  return `${path}?${params.toString()}`;
}

export function signInUrl(location: Pick<Location, 'pathname' | 'search'>): string {
  return `${LOGIN_PATH}?${new URLSearchParams({ target_to: currentReturnTarget(location) }).toString()}`;
}

export function AdminSignInRedirect({ redirect = replaceLocation }: AdminSignInRedirectProps = {}): ReactNode {
  const alreadyTried = new URLSearchParams(window.location.search).has(RETRY_PARAM);
  const url = signInUrl(window.location);

  useEffect(() => {
    if (!alreadyTried) redirect(url);
  }, [alreadyTried, redirect, url]);

  return (
    <StatusPage
      testId="admin-sign-in-redirect"
      title={
        alreadyTried
          ? t('pages.admin.signIn.retryTitle', 'Sign in to continue')
          : t('pages.admin.signIn.title', 'Taking you to sign in…')
      }
      actions={
        alreadyTried ? (
          <Button variant="contained" component="a" href={url}>
            {t('pages.admin.signIn.button', 'Sign in')}
          </Button>
        ) : undefined
      }
    />
  );
}
