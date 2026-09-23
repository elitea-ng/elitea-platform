/**
 * ui/CredentialWarningBanner.tsx — inline warning shown when a shared
 * toolkit references a private credential the current user's personal
 * project does not have, with a "Create a credential" link pre-filled with
 * the required id. Ported from
 * `apps/elitea-ui/src/components/CredentialWarningBanner.jsx`.
 *
 * DEVIATIONS (disclosed, both forced by ownership scope — see the A7 task
 * brief's cross-cutting-hazard note):
 *  - The baseline builds its "create credential" URL from the old
 *    `react-router` `RouteDefinitions`/`getBasename()`/`useSelectedProjectId`
 *    /Redux `personal_project_id` — none of which this unit may reach for
 *    (no session/project store exists yet anywhere in the app; see this
 *    unit's final report). Redesigned as an explicit `buildCreateHref`
 *    prop (defaulting to a same-router `/credentials/create-credential`
 *    path built from `personalProjectId`, injected by the caller) — the
 *    caller (a toolkit-attachment screen, outside this unit's scope) is
 *    the one that actually knows the app's real router base and current
 *    project.
 *  - The baseline also calls `useEliteaAssistantRef().current?.showPopup()`
 *    on mount (`widgets/support-assistant`, cross-domain, not owned by this
 *    unit). Dropped; an optional `onMount` callback is exposed instead so a
 *    caller that owns that widget can wire the same behaviour without this
 *    component reaching into another slice.
 */
import { useEffect, useRef, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ErrorIcon } from '@/shared/ui/icons/error-icon';

export interface CredentialWarningBannerProps {
  readonly credentialId?: string;
  readonly credentialType?: string;
  readonly section?: string;
  /** Personal-project-scoped "create credential" URL, already carrying `prefill_id`/`prefill_name`/`section`. Built by the caller — see this file's doc comment. */
  readonly createHref: string;
  readonly onMount?: () => void;
}

export function CredentialWarningBanner({ credentialId, credentialType, section: _section, createHref, onMount }: CredentialWarningBannerProps): ReactNode {
  const hasFiredOnMount = useRef(false);

  useEffect(() => {
    if (hasFiredOnMount.current) return;
    hasFiredOnMount.current = true;
    onMount?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fire-once-on-mount, matches the baseline's own `[]` dependency array.
  }, []);

  const requiresText = credentialType
    ? t('credentials.warningBanner.requiresType', ' This toolkit requires your own private {{credentialType}} credentials. ', { credentialType })
    : ' ';
  const matchingIdText = credentialId
    ? t('credentials.warningBanner.matchingId', ' with the matching ID "{{credentialId}}" in your Private workspace to use this toolkit.', { credentialId })
    : t('credentials.warningBanner.matching', ' in your Private workspace to use this toolkit.');

  return (
    <Box sx={containerSx}>
      <Box
        component={ErrorIcon}
        sx={iconSx}
      />
      <Typography
        variant="bodySmall"
        sx={textSx}
      >
        <strong>{t('credentials.warningBanner.title', 'Credential setup required:')}</strong>
        {requiresText}
        <Link
          href={createHref}
          target="_blank"
          rel="noreferrer"
          sx={linkSx}
        >
          {t('credentials.warningBanner.link', 'Create a credential')}
        </Link>
        {matchingIdText}
      </Typography>
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'flex-start',
  gap: theme.spacing(1.5),
  padding: theme.spacing(1, 1.5),
  backgroundColor: theme.vars.palette.background.errorBkg,
  border: `0.0625rem solid ${theme.vars.palette.border.error}`,
  borderRadius: theme.vars.shape.radiusMd,
  marginTop: theme.spacing(1),
});

const iconSx: SxProps<Theme> = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.icon.fill.error,
  flexShrink: 0,
  marginTop: '0.1rem',
});

const textSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  color: theme.vars.palette.text.warningText,
  wordBreak: 'break-word',
});

const linkSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.createButton,
  textDecorationColor: theme.vars.palette.text.createButton,
  '&:hover': { color: theme.vars.palette.text.createButton },
});
