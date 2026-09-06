/**
 * Profile page (settings tab) — the PERSONAL section's first tab.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/profile/Profile.jsx`.
 * Read-only: the editable half of the old combined profile screen now lives
 * on Preferences / AI Personality / Memory, so this page carries only the
 * identity rows and the Log out button.
 *
 * The header row is the ROUTE's `DrawerPageHeader` (see `profile.tsx`), not a
 * second one here — the four sibling settings pages each rendered their own
 * on top of the route's, which drew the title twice.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { profileFeature } from '@/features/settings';

const { ProfileIdentity } = profileFeature;

interface AuthorIdentity {
  id?: string;
  name?: string;
  email?: string;
  avatar?: string;
}

const Profile = memo(() => {
  const { data: authorResponse, isLoading, isFetching } = useGetCurrentAuthor();
  const author = authorResponse?.data as AuthorIdentity | undefined;

  return (
    <Box sx={contentSx}>
      <ProfileIdentity
        name={author?.name ?? ''}
        email={author?.email ?? ''}
        avatar={author?.avatar ?? ''}
        userId={author?.id ?? ''}
        isFetching={isLoading || isFetching}
      />
    </Box>
  );
});

Profile.displayName = 'Profile';

export default Profile;

const contentSx: SxProps<Theme> = (theme) => ({
  // Baseline `Profile.jsx`'s `content`: the settings body sits on the panel
  // surface, not the page surface.
  backgroundColor: theme.vars.palette.background.tabPanel,
  height: '100%',
  width: '100%',
  overflowY: 'auto',
  display: 'flex',
  justifyContent: 'center',
});
