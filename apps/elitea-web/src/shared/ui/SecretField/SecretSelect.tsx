import { useCallback, useRef, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import FormHelperText from '@mui/material/FormHelperText';
import IconButton from '@mui/material/IconButton';
import InputLabel from '@mui/material/InputLabel';
import ListSubheader from '@mui/material/ListSubheader';
import MenuItem from '@mui/material/MenuItem';
import Select, { type SelectChangeEvent } from '@mui/material/Select';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { RefreshIcon } from '../icons/refresh-icon';
import { t } from '@/shared/i18n';

import type { SecretFieldSecretsOptions } from './SecretField.types';

/**
 * The "pick an existing secret" half of `SecretField`, split into its own
 * file purely to keep that file under the §3.5 400-line budget — see its own
 * doc comment for the full component's design rationale.
 */

/** The sentinel `Select` value for the "Create new secret" row — exported so `SecretField.tsx`'s `handleSecretChange` can detect it without duplicating the literal. */
export const CREATE_SECRET_VALUE = '__create_secret__';

interface SecretSelectProps {
  name: string | undefined;
  label: string;
  value: string;
  onChange: (event: SelectChangeEvent<string>) => void;
  secrets: SecretFieldSecretsOptions;
  disabled: boolean;
  required: boolean;
  error: boolean | undefined;
  helperText: string | undefined;
}

/** The "pick an existing secret" entry, split out for the same reason as `PasswordField`. */
export function SecretSelect({
  name,
  label,
  value,
  onChange,
  secrets,
  disabled,
  required,
  error,
  helperText,
}: SecretSelectProps): ReactNode {
  const labelId = name ? `${name}-label` : 'secret-field-select-label';
  const canCreate = Boolean(secrets.canCreate && secrets.onCreate);
  const refreshLabel = t('shared.ui.secretField.refreshTooltip', 'Refresh secrets');

  // #926/ELITEA-1071: the "Create new secret" shortcut opens a NEW TAB
  // (`secrets.onCreate`, a `window.open`), which steals focus from this
  // page. An uncontrolled MUI `Select` closes its popup on ANY item
  // selection as part of its own built-in behaviour — nothing to do with
  // the new tab specifically — so the create action always closed the
  // dropdown along with everything else. Made controlled here so the
  // create action's own `onClose` request can be swallowed once, keeping
  // the dropdown open the way selecting a REAL secret never needed to.
  const [open, setOpen] = useState(false);
  const keepOpenRef = useRef(false);
  const handleChange = useCallback(
    (event: SelectChangeEvent<string>) => {
      if (event.target.value === CREATE_SECRET_VALUE) keepOpenRef.current = true;
      onChange(event);
    },
    [onChange],
  );
  const handleClose = useCallback(() => {
    if (keepOpenRef.current) {
      keepOpenRef.current = false;
      return;
    }
    setOpen(false);
  }, []);

  return (
    <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: (theme: Theme) => theme.spacing(1), flex: 1 }}>
      <FormControl
        variant="standard"
        fullWidth
        disabled={disabled}
        required={required}
        error={error}
      >
        <InputLabel id={labelId}>{label}</InputLabel>
        <Select<string>
          labelId={labelId}
          id={name}
          name={name}
          value={value}
          onChange={handleChange}
          open={open}
          onOpen={() => setOpen(true)}
          onClose={handleClose}
        >
          {canCreate
            ? [
                <MenuItem
                  key={CREATE_SECRET_VALUE}
                  value={CREATE_SECRET_VALUE}
                >
                  {secrets.createLabel ?? t('shared.ui.secretField.createSecret', 'Create new secret')}
                </MenuItem>,
                <ListSubheader key="saved-secrets-header">
                  {t('shared.ui.secretField.savedSecrets', 'Saved secrets')}
                </ListSubheader>,
              ]
            : null}
          {(secrets.options ?? []).map((option) => (
            <MenuItem
              key={option.value}
              value={option.value}
            >
              {option.label}
            </MenuItem>
          ))}
        </Select>
        {helperText && <FormHelperText>{helperText}</FormHelperText>}
      </FormControl>
      {secrets.onRefresh && (
        <Tooltip
          title={refreshLabel}
          placement="top"
        >
          <IconButton
            aria-label={refreshLabel}
            size="small"
            disabled={disabled}
            onClick={secrets.onRefresh}
          >
            <RefreshIcon />
          </IconButton>
        </Tooltip>
      )}
    </Box>
  );
}
