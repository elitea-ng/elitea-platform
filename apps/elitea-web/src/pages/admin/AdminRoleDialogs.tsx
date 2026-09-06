/**
 * The two dialogs behind role create / rename / delete on Admin › Roles
 * (gap G9).
 *
 * The page's matrix edits the CELLS of a table whose columns already exist.
 * These change the columns: `POST | PUT | DELETE /admin/roles/{scope}/{mode}`.
 *
 * ## Client-side rules are a courtesy, never the gate
 *
 * `internal/api/v2/admin/roles_crud.go` validates the name, refuses every
 * built-in role as a rename or delete target, and refuses a delete while the
 * role still holds members — every one of those independently of what this file
 * does. The checks here exist so a control does not appear to work and then
 * fail, which is the same division `PermissionMatrixRows` already draws for the
 * `system` column. Nothing here is allowed to be the only thing standing
 * between an operator and a mutation.
 *
 * ## Why deleting asks for the ROLE NAME and not a fixed word
 *
 * `AdminProjectDeleteDialog` asks for the literal word DELETE, because it can
 * destroy several projects at once and a per-project prompt would be unusable.
 * This dialog deletes exactly one named thing, so the name is the stronger
 * prompt: typing it proves the operator read WHICH role they are about to take
 * away, which is the mistake that matters when four tabs show four different
 * role lists that share the same names.
 */
import { useCallback, useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/**
 * The names the SERVER refuses as a rename or delete target, mirrored here so
 * the controls are not offered for them.
 *
 * They are not a naming convention: `services/elitea-main` writes every one of
 * them as a literal in its own SQL and Go (`adminRolePriority`,
 * `usersPageQuery`'s `r.name = 'super_admin'`, `projectprovisioning`'s
 * `systemProjectRole`), so a rename would leave the platform looking for a role
 * that no longer exists. `roles_crud.go`'s `builtInRoles` is the enforcement.
 */
const BUILT_IN_ROLES: readonly string[] = [
  'system',
  'super_admin',
  'admin',
  'editor',
  'viewer',
];

export function isBuiltInRole(role: string): boolean {
  return BUILT_IN_ROLES.includes(role);
}

/**
 * The name rule, mirroring `roles_crud.go`'s `roleNamePattern`.
 *
 * `name` is excluded separately: the matrix response puts the PERMISSION under
 * the key `name` and each role under its own key, so a role called `name` would
 * replace the permission name in every row of the matrix.
 */
const ROLE_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

function roleNameProblem(name: string, existing: readonly string[]): string | undefined {
  const trimmed = name.trim();
  if (trimmed === '') return undefined; // not an error yet, just not submittable
  if (trimmed === 'name') {
    return t(
      'pages.admin.roles.name.reserved',
      '“name” is reserved — the permission matrix uses it for each row’s permission.',
    );
  }
  if (!ROLE_NAME_PATTERN.test(trimmed)) {
    return t(
      'pages.admin.roles.name.invalid',
      'Use 1 to 64 letters, digits, underscores or hyphens, starting with a letter or digit.',
    );
  }
  if (existing.includes(trimmed)) {
    return t('pages.admin.roles.name.taken', 'A role with this name already exists here.');
  }
  return undefined;
}

export type RoleNameDialogMode = 'create' | 'rename';

export interface RoleNameDialogProps {
  readonly open: boolean;
  readonly mode: RoleNameDialogMode;
  /** The role being renamed. Empty for a create. */
  readonly role: string;
  /** The roles the current tab already defines — used for the duplicate hint. */
  readonly existingRoles: readonly string[];
  readonly isSubmitting: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly failureReason: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (name: string) => void;
}

export function RoleNameDialog({
  open,
  mode,
  role,
  existingRoles,
  isSubmitting,
  failureReason,
  onClose,
  onSubmit,
}: RoleNameDialogProps) {
  const [name, setName] = useState('');

  useEffect(() => {
    if (open) setName(mode === 'rename' ? role : '');
  }, [open, mode, role]);

  // A rename must not collide with the role being renamed itself.
  const others = existingRoles.filter((candidate) => candidate !== role);
  const problem = roleNameProblem(name, mode === 'rename' ? others : existingRoles);
  const trimmed = name.trim();
  const armed =
    trimmed !== '' && problem === undefined && !isSubmitting && !(mode === 'rename' && trimmed === role);

  const handleSubmit = useCallback(() => {
    if (!armed) return;
    onSubmit(name.trim());
  }, [armed, name, onSubmit]);

  return (
    <Dialog open={open} onClose={isSubmitting ? undefined : onClose} maxWidth="xs" fullWidth>
      <DialogTitle>
        {mode === 'create'
          ? t('pages.admin.roles.create.title', 'New role')
          : t('pages.admin.roles.rename.title', 'Rename role')}
      </DialogTitle>
      <DialogContent>
        {mode === 'create' ? (
          <Typography variant="bodyMedium" sx={{ display: 'block', marginBottom: '0.75rem' }}>
            {t(
              'pages.admin.roles.create.body',
              'The role starts with no permissions. Grant them on this page, then use “Apply to Projects” to push a standard role out to existing projects.',
            )}
          </Typography>
        ) : (
          <Typography variant="bodyMedium" sx={{ display: 'block', marginBottom: '0.75rem' }}>
            {t(
              'pages.admin.roles.rename.body',
              'Permissions and member assignments follow the role. A standard role is renamed in every project that holds it.',
            )}
          </Typography>
        )}

        {failureReason ? (
          <Alert severity="error" sx={{ marginBottom: '0.75rem' }} data-testid="admin-roles-dialog-error">
            {failureReason}
          </Alert>
        ) : null}

        <TextField
          fullWidth
          margin="dense"
          value={name}
          disabled={isSubmitting}
          error={problem !== undefined}
          helperText={problem}
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') handleSubmit();
          }}
          label={t('pages.admin.roles.name.label', 'Role name')}
          slotProps={{ htmlInput: { 'aria-label': 'Role name' } }}
        />
      </DialogContent>
      <DialogActions sx={{ paddingX: '1.5rem', paddingBottom: '1rem' }}>
        <Button variant="elitea" color="tertiary" onClick={onClose} disabled={isSubmitting}>
          {t('pages.admin.roles.dialog.cancel', 'Cancel')}
        </Button>
        <Button variant="elitea" color="primary" onClick={handleSubmit} disabled={!armed}>
          {mode === 'create'
            ? t('pages.admin.roles.create.submit', 'Create role')
            : t('pages.admin.roles.rename.submit', 'Rename role')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export interface RoleDeleteDialogProps {
  readonly open: boolean;
  readonly role: string;
  readonly isSubmitting: boolean;
  /**
   * The server's own words. A refused delete is usually the 409 that names how
   * many members still hold the role, and that count is the thing the operator
   * acts on — replacing it with "Failed to delete" would hide it.
   */
  readonly failureReason: string | undefined;
  readonly onClose: () => void;
  readonly onConfirm: () => void;
}

export function RoleDeleteDialog({
  open,
  role,
  isSubmitting,
  failureReason,
  onClose,
  onConfirm,
}: RoleDeleteDialogProps) {
  const [confirmation, setConfirmation] = useState('');

  useEffect(() => {
    if (open) setConfirmation('');
  }, [open, role]);

  const armed = confirmation.trim() === role && !isSubmitting;

  return (
    <Dialog open={open} onClose={isSubmitting ? undefined : onClose} maxWidth="xs" fullWidth>
      <DialogTitle>{t('pages.admin.roles.delete.title', 'Delete role')}</DialogTitle>
      <DialogContent>
        <Alert severity="warning" sx={{ marginBottom: '1rem' }}>
          <AlertTitle>{t('pages.admin.roles.delete.warningTitle', 'This removes the role')}</AlertTitle>
          {t(
            'pages.admin.roles.delete.warningBody',
            'Every permission granted to this role goes with it. A role that is still assigned to anyone is refused, and the response says how many assignments remain.',
          )}
        </Alert>

        {failureReason ? (
          <Alert severity="error" sx={{ marginBottom: '1rem' }} data-testid="admin-roles-delete-error">
            {failureReason}
          </Alert>
        ) : null}

        <TextField
          fullWidth
          margin="dense"
          value={confirmation}
          disabled={isSubmitting}
          onChange={(event) => setConfirmation(event.target.value)}
          label={t('pages.admin.roles.delete.confirmLabel', 'Type {{role}} to confirm', { role })}
          slotProps={{ htmlInput: { 'aria-label': `Type ${role} to confirm` } }}
        />
      </DialogContent>
      <DialogActions sx={{ paddingX: '1.5rem', paddingBottom: '1rem' }}>
        <Button variant="elitea" color="tertiary" onClick={onClose} disabled={isSubmitting}>
          {t('pages.admin.roles.dialog.cancel', 'Cancel')}
        </Button>
        <Button color="error" variant="contained" onClick={onConfirm} disabled={!armed}>
          {isSubmitting
            ? t('pages.admin.roles.delete.submitting', 'Deleting…')
            : t('pages.admin.roles.delete.submit', 'Delete role')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
