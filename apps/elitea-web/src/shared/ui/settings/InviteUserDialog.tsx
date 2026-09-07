/**
 * Dialog for inviting users by email — one role set, many addresses.
 *
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/settings/components/InviteUserDialog.jsx`.
 *
 * ## What changed, and why each change is a defect fix rather than a redesign
 *
 * The transport was already bulk: `POST /admin/users/default/{id}` takes
 * `{emails: [...], roles: [...]}` and answers one row per address. Three things
 * on this side threw that away.
 *
 *  1. **The addresses were invisible.** The field is a comma-separated string,
 *     so an operator pasting twelve addresses could not see how many the dialog
 *     had actually parsed, or which one was malformed — only a single "Invalid
 *     email: …" line. They are now chips, one per address, and the invalid ones
 *     say so individually.
 *  2. **The selected roles were invisible.** `SingleSelect` was mounted with
 *     `value=""` and an onChange that toggled a hidden array, so picking a role
 *     changed nothing on screen and picking it twice silently removed it. The
 *     select still drives the toggle — its `select-option-*` items are what the
 *     journey clicks — and the chosen roles are now rendered beside it.
 *  3. **The per-address answer was discarded.** The server distinguishes
 *     invited / already a member / invalid address / failed for EVERY address.
 *     The page collapsed the array into one toast, so a partial failure looked
 *     like a total one. `results` renders that array, and the dialog stays open
 *     while any row needs reading.
 *
 * Deviations from the baseline kept as they were: `InputBase` uses
 * `expand: { maxRows }` for multiline, and the MUI `TextFieldProps` onChange
 * shape.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BaseBtn } from '../BaseBtn';
import { InputBase } from '../InputBase';
import { SingleSelect } from '../SingleSelect';
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '../SingleSelectMenuItem';
import {
  invalidInviteAddresses,
  isValidInviteAddress,
  parseInviteAddresses,
  type InviteAddressResult,
  type InviteOutcome,
} from './inviteResults';

export interface InviteUserDialogProps {
  open: boolean;
  onClose: () => void;
  rolesOptions: SingleSelectOption[];
  onConfirm: (data: { emails: string[]; roles: string[] }) => void;
  /**
   * The per-address answer to the LAST submit, or nothing before the first one.
   * Owned by the page, because the page owns the mutation; the dialog only
   * renders it.
   */
  results?: readonly InviteAddressResult[];
}

/**
 * Outcome → the word shown on the row, and the MUI colour that carries it.
 *
 * `already_member` is `default`, not `error`: the address is in the project,
 * which is the state the operator wanted. Only the two rows an operator must
 * act on are coloured as failures.
 */
const OUTCOME_LABEL: Record<InviteOutcome, { text: string; colour: 'success' | 'default' | 'error' }> = {
  invited: {
    text: t('shared.ui.settings.users.outcome.invited', 'Invited'),
    colour: 'success',
  },
  already_member: {
    text: t('shared.ui.settings.users.outcome.alreadyMember', 'Already a member'),
    colour: 'default',
  },
  invalid_email: {
    text: t('shared.ui.settings.users.outcome.invalidEmail', 'Invalid address'),
    colour: 'error',
  },
  failed: {
    text: t('shared.ui.settings.users.outcome.failed', 'Not added'),
    colour: 'error',
  },
};

export const InviteUserDialog = ({
  open,
  onClose,
  rolesOptions,
  onConfirm,
  results,
}: InviteUserDialogProps) => {
  const [inputText, setInputText] = useState('');
  const [selectedRoles, setSelectedRoles] = useState<string[]>([]);
  const [error, setError] = useState(false);
  const [helperText, setHelperText] = useState('');

  useEffect(() => {
    if (!open) {
      setInputText('');
      setSelectedRoles([]);
      setError(false);
      setHelperText('');
    }
  }, [open]);

  const emails = useMemo(() => parseInviteAddresses(inputText), [inputText]);

  const invalid = useMemo(() => invalidInviteAddresses(emails), [emails]);
  const hasError = invalid.length > 0;

  // The message keeps the reference wording ("Invalid email: a, b") verbatim.
  // The chips below say the same thing per address; this line is what the
  // journey and every screen reader announce, so it is not replaced by them.
  const invalidMessage = useMemo(
    () => (invalid.length > 0 ? `Invalid email: ${invalid.join(', ')}` : ''),
    [invalid],
  );

  useEffect(() => {
    if (hasError) {
      setError(true);
      setHelperText(invalidMessage);
    }
  }, [hasError, invalidMessage]);

  const handleBlur = useCallback(() => {
    setError(hasError);
    setHelperText(invalidMessage);
  }, [hasError, invalidMessage]);

  const handleRolesChange = useCallback((value: string) => {
    setSelectedRoles(prev => {
      if (prev.includes(value)) return prev.filter(r => r !== value);
      return [...prev, value];
    });
  }, []);

  const handleRemoveRole = useCallback((value: string) => {
    setSelectedRoles(prev => prev.filter(r => r !== value));
  }, []);

  const handleConfirm = useCallback(() => {
    onConfirm({ emails, roles: selectedRoles });
  }, [onConfirm, emails, selectedRoles]);

  const roleLabel = useCallback(
    (value: string) => rolesOptions.find(option => option.value === value)?.label ?? value,
    [rolesOptions],
  );

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
        {t('shared.ui.settings.users.inviteUsers', 'Invite users')}
      </DialogTitle>
      <DialogContent sx={contentSx}>
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {t(
            'shared.ui.settings.users.inviteUsersDescription',
            'Enter user emails (separated by comma) and select roles to define permissions for this project.',
          )}
        </Typography>
        <InputBase
          label={t('shared.ui.settings.users.emails', 'Emails')}
          value={inputText}
          onChange={e => setInputText(e.target.value)}
          onBlur={handleBlur}
          expand={{ maxRows: 10 }}
          fullWidth
          required
          sx={inputSx}
        />
        {emails.length > 0 && (
          <Box
            data-testid="invite-email-chips"
            sx={chipRowSx}
          >
            {emails.map(email => (
              <Chip
                key={email}
                size="small"
                label={email}
                data-testid={`invite-email-chip-${email}`}
                color={isValidInviteAddress(email) ? 'default' : 'error'}
                variant={isValidInviteAddress(email) ? 'outlined' : 'filled'}
              />
            ))}
          </Box>
        )}
        {error && (
          <Typography variant="bodySmall" color="error">
            {helperText}
          </Typography>
        )}
        <SingleSelect
          value=""
          onChange={handleRolesChange}
          options={rolesOptions}
          label={t('shared.ui.settings.users.roles', 'Roles')}
          sx={selectSx}
        />
        {selectedRoles.length > 0 && (
          <Box
            data-testid="invite-role-chips"
            sx={chipRowSx}
          >
            {selectedRoles.map(role => (
              <Chip
                key={role}
                size="small"
                label={roleLabel(role)}
                data-testid={`invite-role-chip-${role}`}
                onDelete={() => handleRemoveRole(role)}
              />
            ))}
          </Box>
        )}
        {results && results.length > 0 && (
          <Box
            data-testid="invite-results"
            component="ul"
            sx={resultsSx}
          >
            {results.map(row => (
              <Box
                key={`${row.email}:${row.outcome}`}
                component="li"
                data-testid={`invite-result-${row.email}`}
                sx={resultRowSx}
              >
                <Chip
                  size="small"
                  label={OUTCOME_LABEL[row.outcome].text}
                  color={OUTCOME_LABEL[row.outcome].colour}
                />
                <Typography variant="bodySmall" component="span">
                  {row.email}
                </Typography>
              </Box>
            ))}
          </Box>
        )}
      </DialogContent>
      <DialogActions sx={actionsSx}>
        <BaseBtn
          variant="secondary"
          onClick={onClose}
        >
          {results && results.length > 0
            ? t('shared.ui.baseModal.close', 'Close')
            : t('shared.ui.baseModal.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="contained"
          color="primary"
          onClick={handleConfirm}
          disabled={emails.length === 0 || selectedRoles.length === 0 || error}
        >
          {t('shared.ui.settings.users.invite', 'Invite')}
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

const inputSx: SxProps<Theme> = {
  flex: 1,
  minHeight: '3.5rem',
};

const selectSx: SxProps<Theme> = {
  marginTop: '0.5rem',
};

const chipRowSx: SxProps<Theme> = {
  display: 'flex',
  flexWrap: 'wrap',
  gap: '0.375rem',
};

const resultsSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.375rem',
  margin: 0,
  padding: 0,
  listStyle: 'none',
};

const resultRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.5rem',
};

const actionsSx: SxProps<Theme> = {
  padding: '1rem 1.5rem',
  gap: '0.75rem',
};
