/**
 * Dialog for editing user roles.
 *
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/settings/components/EditUserRolesDialog.jsx`.
 *
 * Deviations:
 *  - Uses MUI `Dialog` directly instead of the FSD `Modal.BaseModal`.
 *  - Uses MUI `Select` with `multiple` for role selection.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import MuiSelect from '@mui/material/Select';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BaseBtn } from '../BaseBtn';
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '../SingleSelectMenuItem';

export interface EditUserRolesDialogProps {
  open: boolean;
  onClose: () => void;
  rolesOptions: SingleSelectOption[];
  originalRoles: string[];
  onConfirm: (roles: string[]) => void;
}

export const EditUserRolesDialog = ({
  open,
  onClose,
  rolesOptions,
  originalRoles,
  onConfirm,
}: EditUserRolesDialogProps) => {
  const [selectedRoleIds, setSelectedRoleIds] = useState<string[]>(originalRoles);

  /**
   * `originalRoles` arrives as a fresh array literal on every parent render —
   * `EditUsersButton` passes `(_userRoles as string[] | undefined) ?? []`, and
   * `useUsersActions` rebuilds the `userRoles` behind it. Keying the reset
   * effect on that identity meant ANY re-render of the Users page while the
   * dialog was open (a react-query refetch flipping `isFetching`, the
   * toast-clear timer) threw away the user's in-dialog selection and re-disabled
   * Save mid-edit — J22f's documented ~1-in-15 failure. So collapse the prop to
   * a value-derived key first, and hang both the reset and the `hasChanged`
   * comparison off that: identity churn is invisible, a genuine change of roles
   * is not.
   */
  const originalRolesKey = useMemo(
    () => JSON.stringify([...originalRoles].sort()),
    [originalRoles],
  );

  // Parsing back what we just serialised round-trips any string array exactly,
  // including edge values a delimiter-joined key would mangle. This is also the
  // same comparison `hasChanged` used before the fix, so the enabled/disabled
  // semantics of Save are unchanged — only what re-triggers the reset is.
  const normalizedOriginalRoles = useMemo(
    () => JSON.parse(originalRolesKey) as string[],
    [originalRolesKey],
  );

  const hasChanged = useMemo(
    () => JSON.stringify([...selectedRoleIds].sort()) !== originalRolesKey,
    [selectedRoleIds, originalRolesKey],
  );

  useEffect(() => {
    setSelectedRoleIds(normalizedOriginalRoles);
  }, [open, normalizedOriginalRoles]);

  const handleConfirm = useCallback(() => {
    onConfirm(selectedRoleIds);
  }, [onConfirm, selectedRoleIds]);

  const handleChange = useCallback((event: unknown) => {
    const value = (event as { target: { value: unknown } }).target.value;
    setSelectedRoleIds(value as string[]);
  }, []);

  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="sm"
      fullWidth
      slotProps={{
        paper: {
          sx: {
            width: '31.25rem',
            maxWidth: '90vw',
          },
        },
      }}
    >
      <DialogTitle>
        {t('shared.ui.settings.users.editRoles', 'Edit roles')}
      </DialogTitle>
      <DialogContent sx={contentSx}>
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {t(
            'shared.ui.settings.users.editRolesDescription',
            'Select the roles to define user permissions for this project.',
          )}
        </Typography>
        <FormControl fullWidth>
          <InputLabel id="roles-select-label">
            {t('shared.ui.settings.users.roles', 'Roles')}
          </InputLabel>
          <MuiSelect
            labelId="roles-select-label"
            multiple
            value={selectedRoleIds}
            onChange={handleChange}
            renderValue={selected => {
              const labels = rolesOptions
                .filter(o => selected.includes(o.value))
                .map(o => o.label);
              return labels.length > 0 ? labels.join(', ') : '';
            }}
            sx={selectSx}
          >
            {rolesOptions.map(option => (
              <MenuItem
                key={option.value}
                value={option.value}
                sx={menuItemSx}
              >
                <BaseCheckbox
                  size="small"
                  checked={selectedRoleIds.includes(option.value)}
                  sx={checkboxSx}
                />
                <Typography variant="bodyMedium">{option.label}</Typography>
              </MenuItem>
            ))}
          </MuiSelect>
        </FormControl>
      </DialogContent>
      <DialogActions sx={actionsSx}>
        <BaseBtn
          variant="secondary"
          onClick={onClose}
        >
          {t('shared.ui.baseModal.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="contained"
          color="primary"
          onClick={handleConfirm}
          disabled={!selectedRoleIds.length || !hasChanged}
        >
          {t('shared.ui.settings.users.save', 'Save')}
        </BaseBtn>
      </DialogActions>
    </Dialog>
  );
};

const contentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
};

const selectSx: SxProps<Theme> = {
  marginTop: '0.5rem',
};

const menuItemSx: SxProps<Theme> = {
  padding: '0.25rem 0.5rem',
  gap: '1rem',
};

const checkboxSx: SxProps<Theme> = {
  padding: '0.25rem',
};

const actionsSx: SxProps<Theme> = {
  padding: '1rem 1.5rem',
  gap: '0.75rem',
};
