/**
 * "Manage project member" — add an address to a project, or change the role of
 * someone already in it (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/ProjectsPage/AddProjectAdminDialog.jsx`
 * (read-only). Rewritten against this app's stack, with four corrections:
 *
 *  1. **The role list comes from the SERVER.** The reference hardcodes
 *     `['admin','editor','viewer']`. elitea-main rejects a role the project
 *     does not define (`resolveProjectRoleIDs` reports it as unknown and the
 *     write is a 400), so a hardcoded list can offer an option guaranteed to
 *     fail. This reads `GET /admin/roles/administration/{projectID}`.
 *  2. **A failure is a failure.** The reference inspects `result[0].status`
 *     on a RESOLVED promise, so a rejected invite still ran the success path's
 *     cache invalidation and reported nothing useful. Here the server's 400 is
 *     a rejection and the message is shown.
 *  3. **The email is validated before it is sent.** The reference has
 *     `type="email"` on a field the browser never validates and no check of its
 *     own, so a typo becomes a server round trip that creates nothing.
 *  4. **The submit button is disabled while the roles are still loading**, so
 *     it cannot post a role that is not in the list yet.
 *
 * The reference's "dialog stays open after success" behaviour IS kept — it lets
 * an operator add several people in a row, which is the actual use.
 *
 * ## Authorisation
 *
 * The three writes (add, change role, remove) are gated SERVER-side on
 * `projects.members.manage` or the legacy `configuration.users.users.create` /
 * `.edit` / `.delete`, resolved from the database in administration mode on
 * every request (`internal/api/router.go`, ADR-0059 R0.6a). An administrator
 * needs no membership of the project. A change that would widen the caller's
 * own permissions is refused (`role_self_escalation`); leaving is not. This dialog is only rendered when the admin-panel
 * config advertises the project-write permission, which is PRESENTATION state —
 * a caller without the permission is refused either way.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useEffect, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';

import {
  useProjectMembers,
  useProjectRoles,
  type AdminProjectRow,
  type ProjectRole,
  type ProjectMemberRow,
} from './api/adminProjectsApi';
import { useProjectMemberWrites } from './projectMemberWrites';

export interface ProjectMemberDialogProps {
  /** The project to act on, or `null` when the dialog is closed. */
  project: AdminProjectRow | null;
  onClose: () => void;
}

/**
 * Deliberately permissive: this is a typo guard, not an address validator.
 * The server owns the real check (`net/mail.ParseAddress` in `validEmail`), and
 * a client rule stricter than the server's would refuse addresses the platform
 * accepts.
 */
function looksLikeEmail(value: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

function readErrorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

/**
 * The default role, once the project's roles are known: `admin` when the
 * project defines it — the reference's default, and the reason this dialog
 * exists — otherwise whatever comes first.
 *
 * Extracted for the same reason `useAdminUsersPage`'s helpers are: inline, its
 * branches push the component past the repo's complexity budget (12), and
 * "which role is preselected" is a fact about the ROLE LIST, not about the
 * dialog's render.
 */
function defaultRoleName(roles: readonly ProjectRole[]): string {
  if (roles.length === 0) return '';
  return roles.some((option) => option.name === 'admin') ? 'admin' : (roles[0]?.name ?? '');
}

/** The dialog's two messages, cleared together whenever a write is attempted. */
interface Feedback {
  readonly error: string;
  readonly success: string;
}

const NO_FEEDBACK: Feedback = { error: '', success: '' };


/** Written once so the field and its test cannot drift apart. */
const EXISTING_MEMBER_HINT = t(
  'pages.admin.projects.member.existing',
  'This user is already in the project \u2014 their role will be replaced.',
);

function findMember(
  members: readonly ProjectMemberRow[] | undefined,
  email: string,
): ProjectMemberRow | undefined {
  if (email === '') return undefined;
  const wanted = email.toLowerCase();
  return (members ?? []).find((member) => member.email.toLowerCase() === wanted);
}

function submitLabel(isUpdate: boolean, isPending: boolean): string {
  if (isPending) return t('pages.admin.projects.member.saving', 'Saving\u2026');
  return isUpdate
    ? t('pages.admin.projects.member.updateRole', 'Update role')
    : t('pages.admin.projects.member.addUser', 'Add user');
}

/**
 * The mutation callbacks, shared by the add and update paths because they
 * differ only in their wording. A rejected write SAYS so — the reference
 * inspects a resolved promise's payload instead and reports nothing useful.
 */
function writeCallbacks(
  isUpdate: boolean,
  setFeedback: (feedback: Feedback) => void,
  clearEmail: () => void,
) {
  return {
    onSuccess: () => {
      setFeedback({
        error: '',
        success: isUpdate
          ? t('pages.admin.projects.member.updated', 'Role updated.')
          : t('pages.admin.projects.member.added', 'User added to the project.'),
      });
      // Cleared so the next address can be typed straight in — the reference's
      // "stay open" behaviour, which is what makes adding several people work.
      clearEmail();
    },
    onError: (error: unknown) => {
      setFeedback({
        success: '',
        error: readErrorMessage(
          error,
          isUpdate
            ? t('pages.admin.projects.member.error.update', 'Failed to update the role.')
            : t('pages.admin.projects.member.error.add', 'Failed to add the user.'),
        ),
      });
    },
  };
}

/**
 * The dialog's three notices. Extracted so the form component stays inside the
 * repo's complexity budget (12) — three independent conditional alerts are
 * three branches, and none of them is about the form's behaviour.
 */
function MemberAlerts({
  feedback,
  rolesFailed,
  onDismiss,
}: {
  readonly feedback: Feedback;
  readonly rolesFailed: boolean;
  readonly onDismiss: () => void;
}) {
  return (
    <>
      {feedback.error ? (
        <Alert severity="error" onClose={onDismiss}>
          {feedback.error}
        </Alert>
      ) : null}
      {feedback.success ? (
        <Alert severity="success" onClose={onDismiss}>
          {feedback.success}
        </Alert>
      ) : null}
      {rolesFailed ? (
        <Alert severity="error">
          {t(
            'pages.admin.projects.member.error.roles',
            'Failed to load this project\u2019s roles, so no role can be assigned.',
          )}
        </Alert>
      ) : null}
    </>
  );
}

function removeLabel(isPending: boolean, armed: boolean): string {
  if (isPending) return t('pages.admin.projects.member.removing', 'Removing…');
  return armed
    ? t('pages.admin.projects.member.removeConfirm', 'Click again to remove')
    : t('pages.admin.projects.member.remove', 'Remove from project');
}

/** Remove the typed member (ADR-0059 R0.6a); its own component for the complexity budget. */
function RemoveMemberButton({
  projectId,
  member,
  disabled,
  setFeedback,
  clearEmail,
}: {
  readonly projectId: number;
  readonly member: ProjectMemberRow | undefined;
  readonly disabled: boolean;
  readonly setFeedback: (feedback: Feedback) => void;
  readonly clearEmail: () => void;
}) {
  const { removeMember } = useProjectMemberWrites();
  // Two clicks: a removal takes every role the member has in the project.
  const [armedFor, setArmedFor] = useState<string | null>(null);
  if (member === undefined) return null;
  const armed = armedFor === member.id;
  const handleRemove = () => {
    if (!armed) {
      setArmedFor(member.id);
      return;
    }
    setArmedFor(null);
    setFeedback(NO_FEEDBACK);
    removeMember.mutate(
      { projectId, userId: member.id },
      {
        onSuccess: () => {
          setFeedback({
            error: '',
            success: t('pages.admin.projects.member.removed', 'User removed from the project.'),
          });
          clearEmail();
        },
        onError: (error: unknown) => {
          setFeedback({
            success: '',
            error: readErrorMessage(
              error,
              t('pages.admin.projects.member.error.remove', 'Failed to remove the user.'),
            ),
          });
        },
      },
    );
  };
  return (
    <Button color="error" onClick={handleRemove} disabled={disabled || removeMember.isPending}>
      {removeLabel(removeMember.isPending, armed)}
    </Button>
  );
}

/**
 * The dialog SHELL only. The form below holds every piece of state and is keyed
 * on the project id, so opening the dialog for a second project cannot inherit
 * the first one's typed address, chosen role or success banner — the leak the
 * reference drawer had between projects.
 */
export function ProjectMemberDialog({ project, onClose }: ProjectMemberDialogProps) {
  return (
    <Dialog open={project !== null} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>{t('pages.admin.projects.member.title', 'Manage project member')}</DialogTitle>
      {project !== null ? (
        <ProjectMemberForm key={project.id} project={project} onClose={onClose} />
      ) : null}
    </Dialog>
  );
}

function ProjectMemberForm({
  project,
  onClose,
}: {
  readonly project: AdminProjectRow;
  readonly onClose: () => void;
}) {
  const [email, setEmail] = useState('');
  const [role, setRole] = useState('');
  const [feedback, setFeedback] = useState<Feedback>(NO_FEEDBACK);

  const projectId = project.id;
  const membersQuery = useProjectMembers(projectId);
  const rolesQuery = useProjectRoles(projectId);
  const { addMember, updateRole } = useProjectMemberWrites();

  const roles = useMemo(() => rolesQuery.data ?? [], [rolesQuery.data]);

  useEffect(() => {
    if (role !== '') return;
    const preselected = defaultRoleName(roles);
    if (preselected !== '') setRole(preselected);
  }, [roles, role]);

  const trimmedEmail = email.trim();

  /**
   * The member this address already names, if any. Matching is
   * case-insensitive because the server normalises addresses before comparing
   * (`normalizeEmail` → `lower(email)`), so a case-different address is the
   * SAME member and an "Add" here would be refused as a duplicate.
   */
  const existingMember = useMemo(
    () => findMember(membersQuery.data, trimmedEmail),
    [membersQuery.data, trimmedEmail],
  );

  const isPending = addMember.isPending || updateRole.isPending;
  const emailLooksWrong = trimmedEmail !== '' && !looksLikeEmail(trimmedEmail);
  const canSubmit =
    looksLikeEmail(trimmedEmail) && role !== '' && !isPending && !rolesQuery.isFetching;

  const handleSubmit = () => {
    if (!canSubmit) return;
    setFeedback(NO_FEEDBACK);
    const callbacks = writeCallbacks(existingMember !== undefined, setFeedback, () => setEmail(''));
    if (existingMember) {
      updateRole.mutate({ projectId, userId: existingMember.id, role }, callbacks);
    } else {
      addMember.mutate({ projectId, email: trimmedEmail, role }, callbacks);
    }
  };

  return (
    <>
      <DialogContent>
        <Stack spacing={2} sx={{ paddingTop: '0.5rem' }}>
          <Typography variant="bodyMedium" color="text.secondary">
            {t('pages.admin.projects.member.subtitle', 'Add or update a user role in')}{' '}
            <strong>{project.name}</strong>
          </Typography>

          <MemberAlerts
            feedback={feedback}
            rolesFailed={rolesQuery.isError}
            onDismiss={() => setFeedback(NO_FEEDBACK)}
          />

          <TextField
            fullWidth
            size="small"
            type="email"
            label={t('pages.admin.projects.member.email', 'User email')}
            placeholder={t('pages.admin.projects.member.emailPlaceholder', 'user@example.com')}
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            error={emailLooksWrong}
            helperText={existingMember ? EXISTING_MEMBER_HINT : ''}
            slotProps={{
              htmlInput: { 'aria-label': t('pages.admin.projects.member.email', 'User email') },
            }}
          />

          <TextField
            select
            fullWidth
            size="small"
            label={t('pages.admin.projects.member.role', 'Role')}
            value={role}
            disabled={roles.length === 0}
            onChange={(event) => setRole(event.target.value)}
          >
            {roles.map((option) => (
              <MenuItem key={option.id} value={option.name}>
                {option.name}
              </MenuItem>
            ))}
          </TextField>
        </Stack>
      </DialogContent>
      <DialogActions>
        <RemoveMemberButton
          projectId={projectId}
          member={existingMember}
          disabled={isPending}
          setFeedback={setFeedback}
          clearEmail={() => setEmail('')}
        />
        <Button onClick={onClose} disabled={isPending}>
          {t('pages.admin.projects.member.cancel', 'Cancel')}
        </Button>
        <Button variant="contained" onClick={handleSubmit} disabled={!canSubmit}>
          {submitLabel(existingMember !== undefined, isPending)}
        </Button>
      </DialogActions>
    </>
  );
}
