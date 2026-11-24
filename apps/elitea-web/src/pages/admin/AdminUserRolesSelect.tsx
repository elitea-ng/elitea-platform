/**
 * The administration roles of one person, as a multi-select (ADR-0059 R0.8c,
 * issue 2586).
 *
 * The menu edits a draft; closing it saves the draft when it differs from what
 * the person holds, so choosing two roles sends ONE `set_admin_role` with both
 * (`role_names`, the whole set). Every role the person holds is shown, also a
 * role the picker would not offer (one that holds nothing), so it can be
 * taken away.
 *
 * Nothing is disabled for the caller's own permissions: the server refuses a
 * role the caller may not give or take, and the page shows why.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import ListItemText from '@mui/material/ListItemText';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { adminRoleDescription, adminRoleLabel } from './adminRoleCopy';
import { isOfferedRole, type AdministrationRole } from './api/adminRoleOptionsApi';

export interface AdminUserRolesSelectProps {
  readonly held: readonly string[];
  readonly roles: readonly AdministrationRole[];
  readonly disabled: boolean;
  readonly onSave: (roleNames: string[]) => void;
}

/** The offered roles, then any held role the matrix does not offer. */
function optionsFor(held: readonly string[], roles: readonly AdministrationRole[]): AdministrationRole[] {
  const options = roles.filter((role) => isOfferedRole(role) || held.includes(role.name));
  for (const name of held) {
    if (!options.some((role) => role.name === name)) {
      options.push({ name, builtIn: false, marker: null, displayName: undefined, holdsPermissions: false });
    }
  }
  return options;
}

function sameSet(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((name) => right.includes(name));
}

export function AdminUserRolesSelect({ held, roles, disabled, onSave }: AdminUserRolesSelectProps) {
  const [draft, setDraft] = useState<string[] | null>(null);
  const options = optionsFor(held, roles);
  const byName = new Map(options.map((role) => [role.name, role]));
  const label = (name: string) => adminRoleLabel(name, byName.get(name));

  const close = () => {
    if (draft !== null && !sameSet(draft, held)) {
      // The options' order, so the request does not depend on the click order.
      onSave(options.map((role) => role.name).filter((name) => draft.includes(name)));
    }
    setDraft(null);
  };

  return (
    <Box onClick={(event) => event.stopPropagation()} sx={{ width: '100%' }}>
      <Select<string[]>
        multiple
        size="small"
        displayEmpty
        open={draft !== null}
        onOpen={() => setDraft([...held])}
        onClose={close}
        value={draft ?? [...held]}
        disabled={disabled}
        inputProps={{ 'aria-label': t('pages.admin.users.column.adminRoles', 'Administration roles') }}
        onChange={(event) => {
          const next = event.target.value;
          setDraft(typeof next === 'string' ? next.split(',') : next);
        }}
        renderValue={(selected) =>
          selected.length === 0 ? t('pages.admin.users.role.none', 'None') : selected.map(label).join(', ')
        }
        sx={{ width: '100%' }}
      >
        {options.map((role) => (
          <MenuItem key={role.name} value={role.name} sx={{ maxWidth: '28rem', whiteSpace: 'normal' }}>
            <BaseCheckbox size="small" checked={(draft ?? held).includes(role.name)} />
            <ListItemText primary={label(role.name)} secondary={adminRoleDescription(role)} />
          </MenuItem>
        ))}
      </Select>
    </Box>
  );
}
