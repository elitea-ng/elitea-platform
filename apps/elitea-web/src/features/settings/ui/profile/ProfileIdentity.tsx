/**
 * ProfileIdentity — the body of Settings › Profile.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/profile/Profile.jsx`.
 * A read-only identity card: avatar + display name, then one label/value row
 * per identity field separated by hairlines, then the "Log out" button.
 *
 * This screen had no counterpart here at all. Its absence is why the settings
 * drawer carried a bare "Log out" NAV ITEM — an action pretending to be a
 * tab, which is neither what the baseline renders nor what the production UI
 * shows. `performLogout()` moves here, onto the button the baseline puts on
 * this page.
 *
 * All four reference rows are here. "Last login" used to be dropped, on the
 * belief that only the admin users list carries `last_login`; the project
 * author list carries it too, for any member, and the caller is always a
 * member of their own personal project. `../../lib/profile/lastLogin.ts`
 * documents the endpoint, the live verification and why the field is read
 * defensively. The row renders whether or not the deployment supplies a
 * value, matching `Profile.jsx:67-70`.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Skeleton from '@mui/material/Skeleton';
import SvgIcon from '@mui/material/SvgIcon';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { performLogout } from '@/shared/api/auth';
import { t } from '@/shared/i18n';
import { LogoutIcon } from '@/shared/ui/icons/logout-icon';

export interface ProfileIdentityProps {
  name: string;
  email: string;
  avatar: string;
  userId: string;
  /** Already formatted for display; `''` when the deployment does not supply one. */
  lastLogin: string;
  isFetching: boolean;
}

/** Deterministic per-name avatar colour — same function `ProfileUserInfo` uses. */
function stringToColor(str: string): string {
  let hash = 0;
  for (let index = 0; index < str.length; index += 1) {
    hash = str.charCodeAt(index) + ((hash << 5) - hash);
  }
  // oxlint-disable-next-line elitea/no-raw-color -- deterministic per-user-name avatar colour, not a brand token.
  return `hsl(${hash % 360}, 50%, 40%)`;
}

function getInitials(name: string): string {
  return name
    .split(' ')
    .map((part) => part.charAt(0).toUpperCase())
    .join('')
    .slice(0, 2);
}

/*
 * THE COLOUR GOES IN `sx`, NOT IN THE `color` PROP.
 *
 * `<Typography color="text.secondary">` emits no colour rule in this MUI
 * setup, so the value cell inherited `text.primary` and every row rendered
 * label and value in the SAME muted grey. Measured against a live
 * deployment the two differ: the label is `rgb(169, 183, 193)`
 * (`text.primary`) and the value is `rgb(255, 255, 255)`
 * (`text.secondary`). The row is a label/value pair; drawing both halves
 * identically removed the only signal that says which is which.
 */
function IdentityRow({ label, value }: { label: string; value: string }) {
  const handleCopy = useCallback(() => {
    void navigator.clipboard?.writeText(value);
  }, [value]);

  return (
    <Box sx={rowSx}>
      <Typography variant="bodyMedium" sx={rowLabelSx}>
        {label}
      </Typography>
      <Tooltip title={t('settings.profile.clickToCopy', 'Click to copy')} placement="top">
        <Typography variant="bodyMedium" onClick={handleCopy} sx={rowValueSx}>
          {value}
        </Typography>
      </Tooltip>
    </Box>
  );
}

export const ProfileIdentity = memo(function ProfileIdentity({
  name,
  email,
  avatar,
  userId,
  lastLogin,
  isFetching,
}: ProfileIdentityProps) {
  const onLogout = useCallback(() => {
    performLogout();
  }, []);

  return (
    <Box sx={wrapperSx}>
      <Box sx={containerSx} data-testid="profile-identity">
        <Box sx={avatarSectionSx}>
          {isFetching ? (
            <Skeleton variant="circular" width={64} height={64} />
          ) : avatar ? (
            <Box component="img" src={avatar} alt={name} sx={avatarImageSx} />
          ) : (
            <Box sx={avatarFallbackSx(stringToColor(name))}>{getInitials(name)}</Box>
          )}
          <Typography variant="labelMedium" sx={nameSx}>
            {name}
          </Typography>
        </Box>

        <Box sx={fieldsSectionSx}>
          <IdentityRow label={t('settings.profile.fullName', 'Full name:')} value={name} />
          <IdentityRow label={t('settings.profile.email', 'Email:')} value={email} />
          <IdentityRow label={t('settings.profile.userId', 'User ID:')} value={userId} />
          <IdentityRow label={t('settings.profile.lastLogin', 'Last login:')} value={lastLogin} />
        </Box>

        <Button
          variant="secondary"
          onClick={onLogout}
          sx={logoutButtonSx}
          startIcon={<SvgIcon component={LogoutIcon} inheritViewBox sx={logoutIconSx} />}
        >
          {t('settings.profile.logout', 'Log out')}
        </Button>
      </Box>
    </Box>
  );
});

const wrapperSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'center',
  width: '100%',
};

const containerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1.5rem',
  padding: '1.5rem',
  maxWidth: '50rem',
  width: '100%',
};

const avatarSectionSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '1rem',
};

const avatarImageSx: SxProps<Theme> = {
  width: '4rem',
  height: '4rem',
  borderRadius: 'var(--el-shape-radiusPill, 9999px)',
  objectFit: 'cover',
};

const avatarFallbackSx =
  (background: string): SxProps<Theme> =>
  (theme) => ({
    backgroundColor: background,
    width: '4rem',
    height: '4rem',
    borderRadius: 'var(--el-shape-radiusPill, 9999px)',
    color: theme.vars.palette.text.secondary,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: theme.typography.headingLarge.fontSize,
  });

/* Same `color=` no-op as `IdentityRow`: production draws the display name in
 * `text.secondary` (white), this app drew it in the inherited muted grey. */
const nameSx: SxProps<Theme> = (theme) => ({
  fontWeight: 600,
  color: theme.vars.palette.text.secondary,
});

const fieldsSectionSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  '& > *': {
    padding: '0.75rem 0',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
  },
});

const rowSx: SxProps<Theme> = {
  width: '100%',
  display: 'flex',
  alignItems: 'center',
};

const rowLabelSx: SxProps<Theme> = (theme) => ({
  width: '8.25rem',
  flexShrink: 0,
  color: theme.vars.palette.text.primary,
});

const rowValueSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  padding: '0 0.5rem',
  borderRadius: 'var(--el-shape-radiusPill, 9999px)',
  '&:hover': {
    cursor: 'pointer',
    backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
  },
});

const logoutButtonSx: SxProps<Theme> = {
  width: '7rem',
  alignSelf: 'flex-start',
};

const logoutIconSx: SxProps<Theme> = {
  width: '1rem',
  height: '1rem',
};
