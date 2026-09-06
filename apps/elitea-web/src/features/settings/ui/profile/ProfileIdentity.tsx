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
 * "Last login" is the one baseline row missing: the baseline reads it off the
 * Redux `user` slice, and the only endpoint in THIS app that carries
 * `last_login` is the ADMIN users list (`pages/admin/AdminUsersTable.tsx`),
 * which a non-administrator cannot call. Showing a blank or a fabricated
 * timestamp would be worse than showing three true rows.
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

function IdentityRow({ label, value }: { label: string; value: string }) {
  const handleCopy = useCallback(() => {
    void navigator.clipboard?.writeText(value);
  }, [value]);

  return (
    <Box sx={rowSx}>
      <Typography variant="bodyMedium" color="text.primary" sx={rowLabelSx}>
        {label}
      </Typography>
      <Tooltip title={t('settings.profile.clickToCopy', 'Click to copy')} placement="top">
        <Typography variant="bodyMedium" color="text.secondary" onClick={handleCopy} sx={rowValueSx}>
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
          <Typography variant="labelMedium" color="text.secondary" sx={nameSx}>
            {name}
          </Typography>
        </Box>

        <Box sx={fieldsSectionSx}>
          <IdentityRow label={t('settings.profile.fullName', 'Full name:')} value={name} />
          <IdentityRow label={t('settings.profile.email', 'Email:')} value={email} />
          <IdentityRow label={t('settings.profile.userId', 'User ID:')} value={userId} />
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

const nameSx: SxProps<Theme> = { fontWeight: 600 };

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

const rowLabelSx: SxProps<Theme> = {
  width: '8.25rem',
  flexShrink: 0,
};

const rowValueSx: SxProps<Theme> = (theme) => ({
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
