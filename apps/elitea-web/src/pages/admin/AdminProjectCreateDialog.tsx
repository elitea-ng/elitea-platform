/**
 * Create-project dialog for the admin Projects page.
 *
 * Reference (read-only):
 * `frontends/admin_ui/frontend/src/pages/ProjectsPage/CreateProjectDialog.jsx`.
 * Same two fields, because the server takes two: everything else on
 * `ProjectCreatePD` is a quota with a default, and a dialog that offered the
 * VCU ceiling as a blank number field would let an operator cap a new project
 * at zero by tabbing past it. Omitting a limit sends nothing and takes
 * `DefaultLimits()`; see `./api/adminProjectsApi.ts`.
 *
 * ## What this dialog does NOT let the caller choose
 *
 * The OWNER and the granted ROLE. Both are server-side facts: the owner is the
 * authenticated caller (`g.auth.id` in pylon, the session principal here), and
 * the role is hardcoded `admin`. Neither is a body field, and offering either
 * here would be a privilege decision made in a form.
 *
 * ## Why a failure is rendered as a LIST
 *
 * Provisioning is nine steps. When one fails the server compensates the rest
 * and answers with both lists, so "could not create the project" is never the
 * whole answer — `step project_pgvector did not complete` is. The steps that
 * SUCCEEDED are dropped from the report (nine green lines bury the one red
 * one); the ones that never ran are kept, because a step that was deliberately
 * held back is the thing an operator has to act on.
 */
import { useCallback, useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type {
  CreateProjectInput,
  ProvisioningFailure,
  ProvisioningStep,
} from './api/adminProjectProvisioningApi';

export interface AdminProjectCreateDialogProps {
  readonly open: boolean;
  readonly isSaving: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly serverError: string | undefined;
  /** The failed forward steps and the failed rollback steps, kept apart. */
  readonly failure: ProvisioningFailure;
  readonly onClose: () => void;
  readonly onSubmit: (input: CreateProjectInput) => void;
}

/**
 * A courtesy check, not a gate. The server resolves each address to a real user
 * and refuses the whole request when one is unknown — and that refusal rolls
 * back every step already taken, so catching a typo here saves an entire
 * provisioning round trip rather than merely a rejection.
 */
/**
 * `freeSolo` means every option comes from the operator, so the option list is
 * permanently empty. Hoisted to module scope rather than written inline: a fresh
 * `[]` on every render is a new identity, which makes the Autocomplete rebuild
 * its listbox each keystroke.
 */
const EMPTY_OPTIONS: readonly string[] = [];

/**
 * One labelled group of failed steps, or nothing when the group is empty.
 *
 * The key is the step's POSITION, not its name. The same step name legitimately
 * appears in both lists — `compensate` undoes every attempted step including
 * the one that failed — so a name key collides the moment a rollback fails too,
 * which is exactly the failure this report exists for.
 */
function StepList({ steps, heading }: { steps: readonly ProvisioningStep[]; heading: string }) {
  if (steps.length === 0) return null;
  return (
    <Box sx={{ marginTop: '0.5rem' }}>
      <Typography variant="bodySmall" sx={{ display: 'block', fontWeight: 600 }}>
        {heading}
      </Typography>
      <Box component="ul" sx={{ margin: '0.25rem 0 0', paddingLeft: '1.25rem' }}>
        {steps.map((step, index) => (
          <Typography key={`${index}-${step.step}`} component="li" variant="bodySmall">
            {step.msg ||
              t('pages.admin.projects.step.notRun', 'step {{step}} did not run', {
                step: step.step,
              })}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}

function looksLikeEmail(value: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

/**
 * elitea_issues: #4626 — an email copied from an external source (a copy
 * button, a formatted document) can carry invisible Unicode format
 * characters (Word Joiner U+2060, zero-width space/non-joiner/joiner, a BOM,
 * a soft hyphen). `String.prototype.trim()` only strips whitespace, and none
 * of these are whitespace, so a hidden character survives `.trim()` and then
 * fails admin matching on the backend with no visible reason. Stripped here,
 * before validation, so what the operator SEES is what gets matched.
 */
function stripInvisibleUnicode(value: string): string {
  return value.replace(/[​-‏⁠﻿­]/g, '');
}

/** @internal exported only for {@link sanitizeEmail}'s own unit test. */
export function sanitizeEmail(value: string): string {
  return stripInvisibleUnicode(value).trim();
}

export function AdminProjectCreateDialog({
  open,
  isSaving,
  serverError,
  failure,
  onClose,
  onSubmit,
}: AdminProjectCreateDialogProps) {
  const [name, setName] = useState('');
  const [adminEmails, setAdminEmails] = useState<string[]>([]);
  const [pendingEmail, setPendingEmail] = useState('');
  const [validationError, setValidationError] = useState('');

  // Reopening must not show the previous attempt's draft or its complaint.
  useEffect(() => {
    if (!open) return;
    setName('');
    setAdminEmails([]);
    setPendingEmail('');
    setValidationError('');
  }, [open]);

  const handleSubmit = useCallback(() => {
    const trimmedName = name.trim();
    if (!trimmedName) {
      setValidationError(
        t('pages.admin.projects.create.nameRequired', 'A project name is required.'),
      );
      return;
    }
    // An address left in the input, never committed to a chip, is one the
    // operator typed and expects to be used. The reference includes it too.
    const typed = sanitizeEmail(pendingEmail);
    const emails = typed ? [...adminEmails, typed] : adminEmails;
    const invalid = emails.filter((email) => !looksLikeEmail(email));
    if (invalid.length > 0) {
      setValidationError(
        t('pages.admin.projects.create.invalidEmail', 'Not a valid email address: {{addresses}}', {
          addresses: invalid.join(', '),
        }),
      );
      return;
    }
    setValidationError('');
    onSubmit({ name: trimmedName, adminEmails: emails });
  }, [name, adminEmails, pendingEmail, onSubmit]);

  const message = validationError || serverError;

  return (
    <Dialog open={open} onClose={isSaving ? undefined : onClose} maxWidth="sm" fullWidth>
      <DialogTitle>{t('pages.admin.projects.create.title', 'Create project')}</DialogTitle>
      <DialogContent>
        {message ? (
          <Alert severity="error" sx={{ marginBottom: '1rem' }}>
            {message}
            <StepList
              steps={failure.steps}
              heading={t('pages.admin.projects.step.forwardHeading', 'Provisioning stopped at:')}
            />
            {/*
              Reported separately, and never merged into the list above. A
              forward failure has already been compensated; a ROLLBACK failure
              is infrastructure left behind that an operator has to clean up by
              hand. Two headings is the whole difference between those.
            */}
            <StepList
              steps={failure.rollback}
              heading={t(
                'pages.admin.projects.step.rollbackHeading',
                'Cleanup did not finish — these may need attention:',
              )}
            />
          </Alert>
        ) : null}

        <TextField
          fullWidth
          margin="dense"
          label={t('pages.admin.projects.create.name', 'Project name')}
          value={name}
          disabled={isSaving}
          onChange={(event) => setName(event.target.value)}
        />

        <Autocomplete
          multiple
          freeSolo
          options={EMPTY_OPTIONS}
          value={adminEmails}
          inputValue={pendingEmail}
          disabled={isSaving}
          onInputChange={(_event, value) => setPendingEmail(value)}
          onChange={(_event, value) => setAdminEmails(value.map(sanitizeEmail))}
          renderValue={(value, getTagProps) =>
            // `getTagProps` supplies its own `key`; destructuring it out is what
            // keeps the spread from overwriting an explicit one (TS2783).
            value.map((option, index) => {
              const { key, ...tagProps } = getTagProps({ index });
              return <Chip key={key} label={option} size="small" {...tagProps} />;
            })
          }
          renderInput={(params) => (
            <TextField
              {...params}
              margin="dense"
              label={t('pages.admin.projects.create.adminEmails', 'Project admin email(s)')}
              placeholder={adminEmails.length === 0 ? 'user@example.com' : ''}
              helperText={t(
                'pages.admin.projects.create.adminEmailsHelp',
                'Press Enter to add each address. Each must already have an account — an unknown address fails the whole request. You are the owner either way.',
              )}
            />
          )}
        />
      </DialogContent>
      <DialogActions sx={{ paddingX: '1.5rem', paddingBottom: '1rem' }}>
        <Button variant="elitea" color="tertiary" onClick={onClose} disabled={isSaving}>
          {t('pages.admin.projects.create.cancel', 'Cancel')}
        </Button>
        <Button variant="elitea" color="primary" onClick={handleSubmit} disabled={isSaving}>
          {isSaving
            ? t('pages.admin.projects.create.submitting', 'Creating…')
            : t('pages.admin.projects.create.submit', 'Create')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
