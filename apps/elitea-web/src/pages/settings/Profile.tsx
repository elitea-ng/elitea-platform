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

import { useGetCurrentAuthor, useListSocialAuthors } from '@/shared/api/generated/social/social';
import { profileFeature } from '@/features/settings';

const { ProfileIdentity, formatLastLogin, selectLastLogin } = profileFeature;

interface AuthorIdentity {
  id?: string;
  name?: string;
  email?: string;
  avatar?: string;
  personal_project_id?: string;
}

/**
 * The five display strings, in one place. Extracted from the component so the
 * chain of `??` fallbacks does not count against its complexity budget — and
 * so "what this page shows when a field is missing" is one readable list.
 */
function identityFields(author: AuthorIdentity | undefined, authors: unknown) {
  return {
    name: author?.name ?? '',
    email: author?.email ?? '',
    avatar: author?.avatar ?? '',
    userId: author?.id ?? '',
    lastLogin: formatLastLogin(selectLastLogin(authors, author?.id)),
  };
}

const Profile = memo(() => {
  const { data: authorResponse, isLoading, isFetching } = useGetCurrentAuthor();
  const author = authorResponse?.data as AuthorIdentity | undefined;

  /*
   * The "Last login:" row's only non-admin source. The caller's PERSONAL
   * project is used deliberately: it is the one project every user is
   * guaranteed to be a member of (so the request cannot 403) and it holds
   * exactly one author row, so the scan below is over a single element. See
   * `features/settings/lib/profile/lastLogin.ts` for the endpoint, the live
   * verification, and why the field is narrowed at runtime instead of typed.
   *
   * The query is disabled until the profile resolves — `useListSocialAuthors`
   * takes the project id in the PATH, so firing it early would request
   * `/social/authors/undefined`.
   */
  const personalProjectId = author?.personal_project_id ?? '';
  const { data: authorsResponse } = useListSocialAuthors(personalProjectId, {
    query: { enabled: personalProjectId !== '' },
  });

  return (
    <Box sx={contentSx}>
      <ProfileIdentity
        {...identityFields(author, authorsResponse?.data)}
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
