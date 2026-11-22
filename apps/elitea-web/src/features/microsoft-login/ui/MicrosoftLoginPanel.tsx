/**
 * The Microsoft sign-in block of the credential form (ADR-0057 decision 6): the Login
 * button, who is signed in and until when, Sign in again, Sign out, and the reason a
 * sign-in did not finish.
 *
 * It renders what `useMicrosoftLogin` and the caller's token store report; it holds no
 * token and starts nothing but through `login`.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { failureMessage } from '../lib/failures';
import type { UseMicrosoftLoginResult } from '../model/useMicrosoftLogin';

export interface MicrosoftLoginPanelProps {
  readonly login: UseMicrosoftLoginResult;
  /** A usable access token is held for this credential. */
  readonly signedIn: boolean;
  /** The token was refused (the check answered 401): the sign-in has to be repeated. */
  readonly needsLogin: boolean;
  /** The credential exists already; before that the button saves it first. */
  readonly saved: boolean;
  /** The person may update the credential in this project (sign-in needs that permission). */
  readonly canLogin: boolean;
  readonly onSignOut: () => void;
}

function formatTime(iso: string): string {
  const time = new Date(iso);
  return Number.isNaN(time.getTime()) ? iso : time.toLocaleString();
}

function progressText(phase: UseMicrosoftLoginResult['phase']): string {
  if (phase === 'saving') return t('features.microsoftLogin.saving', 'Saving the credential...');
  if (phase === 'starting') return t('features.microsoftLogin.starting', 'Starting the sign-in...');
  if (phase === 'redeeming') return t('features.microsoftLogin.redeeming', 'Finishing the sign-in...');
  return t('features.microsoftLogin.waiting', 'Waiting for you to sign in in the Microsoft window...');
}

function SignedInSummary({ login, onSignOut }: Pick<MicrosoftLoginPanelProps, 'login' | 'onSignOut'>): ReactNode {
  const { session } = login;
  return (
    <Box
      sx={summarySx}
      data-testid="microsoft-login-summary"
    >
      <Typography variant="bodyMedium">
        {session
          ? t('features.microsoftLogin.signedInAs', 'Signed in as {{account}}', { account: session.account })
          : t('features.microsoftLogin.signedIn', 'Signed in')}
      </Typography>
      {session && (
        <>
          <Typography
            variant="bodySmall"
            color="text.secondary"
          >
            {t('features.microsoftLogin.tenant', 'Microsoft tenant: {{tenant}}', { tenant: session.tenant })}
          </Typography>
          <Typography
            variant="bodySmall"
            color="text.secondary"
          >
            {t('features.microsoftLogin.runsActAs', 'Runs act as {{account}}', { account: session.account })}
          </Typography>
          <Typography
            variant="bodySmall"
            color="text.secondary"
          >
            {t('features.microsoftLogin.expires', 'The sign-in lasts until {{time}}. After that, sign in again.', { time: formatTime(session.expiresAt) })}
          </Typography>
        </>
      )}
      <Box sx={rowSx}>
        <BaseBtn
          variant="secondary"
          onClick={login.login}
          disabled={login.busy}
          data-testid="microsoft-login-again"
        >
          {t('features.microsoftLogin.signInAgain', 'Sign in again')}
        </BaseBtn>
        <BaseBtn
          variant="tertiary"
          color="tertiary"
          onClick={onSignOut}
          disabled={login.busy}
          data-testid="microsoft-login-sign-out"
        >
          {t('features.microsoftLogin.signOut', 'Sign out')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

export function MicrosoftLoginPanel({ login, signedIn, needsLogin, saved, canLogin, onSignOut }: MicrosoftLoginPanelProps): ReactNode {
  if (!canLogin) {
    return (
      <Typography
        variant="bodySmall"
        color="text.secondary"
        data-testid="microsoft-login-managed"
      >
        {t('features.microsoftLogin.managedByAdmin', 'Login managed by a project admin')}
      </Typography>
    );
  }
  return (
    <Box
      sx={panelSx}
      data-testid="microsoft-login-panel"
    >
      <Typography variant="bodySmall">{t('features.microsoftLogin.title', 'Microsoft sign-in')}</Typography>
      {needsLogin && !login.busy && (
        <Typography
          variant="labelSmall"
          sx={noticeSx}
          data-testid="microsoft-login-expired"
        >
          {t('features.microsoftLogin.needsLogin', 'Microsoft refused the saved sign-in (it has ended or lost a permission). Sign in again.')}
        </Typography>
      )}
      {signedIn && !login.busy && !needsLogin ? (
        <SignedInSummary
          login={login}
          onSignOut={onSignOut}
        />
      ) : (
        <Box sx={rowSx}>
          <BaseBtn
            variant="elitea"
            onClick={login.login}
            disabled={login.busy}
            loading={login.busy}
            data-testid="microsoft-login-button"
          >
            {saved
              ? t('features.microsoftLogin.login', 'Sign in with Microsoft')
              : t('features.microsoftLogin.saveAndLogin', 'Save and sign in with Microsoft')}
          </BaseBtn>
          {login.busy && (
            <>
              <Typography
                variant="labelSmall"
                color="text.secondary"
                data-testid="microsoft-login-progress"
              >
                {progressText(login.phase)}
              </Typography>
              <BaseBtn
                variant="tertiary"
                color="tertiary"
                onClick={login.cancel}
                data-testid="microsoft-login-cancel"
              >
                {t('features.microsoftLogin.cancel', 'Cancel')}
              </BaseBtn>
            </>
          )}
        </Box>
      )}
      {login.failure !== undefined && (
        <Typography
          variant="labelSmall"
          sx={errorSx}
          role="alert"
          data-testid="microsoft-login-error"
        >
          {failureMessage(login.failure)}
        </Typography>
      )}
    </Box>
  );
}

const panelSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(1), paddingTop: theme.spacing(1) });
const summarySx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(0.5) });
const rowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1), flexWrap: 'wrap' });
const errorSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.rejected });
const noticeSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.rejected });
