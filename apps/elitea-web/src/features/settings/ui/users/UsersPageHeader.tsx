// @ts-nocheck
/**
 * UsersPageHeader — search bar and action buttons for the users page.
 *
 * Extracted from `UsersPageContent.tsx` to keep that file under 400 lines.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { UserRecord } from '@/shared/api/generated/model';
import { DeleteUserButton } from '@/shared/ui/settings/DeleteUserButton';
import { EditUsersButton } from '@/shared/ui/settings/EditUsersButton';
import type { EditUsersButtonProps } from '@/shared/ui/settings/EditUsersButton';
import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

export function UsersPageHeader({
  usersPageStyles, searchText, onSearchChange,
  actions, selectedUsers, onSetInviteOpen, permissions,
}: {
  usersPageStyles: typeof import('./UsersPage.styles').usersPageStyles;
  searchText: string;
  onSearchChange: (e: React.ChangeEvent<HTMLInputElement>) => void;
  actions: { edit: EditUsersButtonProps | null; delete: Record<string, unknown> };
  selectedUsers: UserRecord[];
  onSetInviteOpen: (open: boolean) => void;
  /** RBAC gates ported from the old app's `checkPermission(PERMISSIONS.users.*)` calls (spec §9.3). */
  permissions: { canView: boolean; canCreate: boolean; canEdit: boolean; canDelete: boolean };
}) {
  return (
    <Box sx={usersPageStyles.header}>
      <Typography variant="headingLarge" component="h1" sx={usersPageStyles.title}>
        {t('shared.ui.settings.users.title', 'Users')}
      </Typography>
      <Box sx={usersPageStyles.toolbar}>
        {permissions.canView && (
          // #6646: the shared search box. This was an outlined TextField with
          // an 18px grey glyph — a third icon size and a hover unlike the rest.
          // The handler takes a change event, so the value is wrapped in one.
          <SimpleSearchBar
            value={searchText}
            onChange={(value) => onSearchChange({ target: { value } } as React.ChangeEvent<HTMLInputElement>)}
            debounceMs={0}
            placeholder={t('shared.ui.settings.users.search', 'Search users…')}
            aria-label={t('shared.ui.settings.users.search', 'Search users…')}
            sx={{ width: 260 }}
          />
        )}
        {actions && selectedUsers.length >= 1 && (
          <>
            {permissions.canEdit && actions.edit && (
              <EditUsersButton {...actions.edit} sx={usersPageStyles.actionButton} />
            )}
            {permissions.canDelete && actions.delete && (
              <DeleteUserButton {...actions.delete} sx={usersPageStyles.actionButton} />
            )}
          </>
        )}
        {permissions.canCreate && (
          <Box sx={usersPageStyles.actionButton}>
            {/*
              * A Button, not an IconButton. `IconButton` is a fixed-size CIRCLE
              * sized for a glyph; this control has a WORD in it, so the label
              * overflowed the circle and rendered as a cyan disc with "nvit"
              * spilling past the right edge of the viewport.
              */}
            <Button
              variant="contained"
              onClick={() => onSetInviteOpen(true)}
              title={t('shared.ui.settings.users.inviteTooltip', 'Invite users')}
            >
              {t('shared.ui.settings.users.invite', 'Invite')}
            </Button>
          </Box>
        )}
      </Box>
    </Box>
  );
}
