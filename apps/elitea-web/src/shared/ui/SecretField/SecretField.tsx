import type { ChangeEvent, MouseEvent, ReactNode } from 'react';
import { useCallback, useState } from 'react';

// [S1-H] Same fallback rationale `BaseModal`'s doc comment records for
// `CloseIcon`, and the exact icons the baseline's own `SecretField.jsx`
// reaches for directly (not one of its custom SVGs): `Visibility`/
// `VisibilityOff` from `@mui/icons-material`.
import Visibility from '@mui/icons-material/Visibility';
import VisibilityOff from '@mui/icons-material/VisibilityOff';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import InputAdornment from '@mui/material/InputAdornment';
import type { SelectChangeEvent } from '@mui/material/Select';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';

import { t } from '@/shared/i18n';

import { CREATE_SECRET_VALUE, SecretSelect } from './SecretSelect';

/** @public Matches the baseline's `{{secret.NAME}}` reference syntax. Exported so a caller can detect the shape without duplicating the pattern. */
export const SECRET_REFERENCE_RE = /^{{secret\.([A-Za-z0-9_]+)}}$/;

/** @public */
export type SecretFieldMode = 'secret' | 'password';

/** @public One entry in {@link SecretFieldSecretsOptions.options}. */
export interface SecretOption {
  label: string;
  value: string;
}

/** @public Everything about the "pick an existing secret" mode — omit entirely to render a plain masked text field with no mode toggle. */
export interface SecretFieldSecretsOptions {
  /** The caller's already-fetched secret list (replaces the baseline's internal `useSecretsListQuery`). */
  options?: SecretOption[];
  /** Caller may create a new secret (e.g. navigate to secret settings). Omit to hide the affordance. */
  onCreate?: () => void;
  /** Permission to create a secret, computed by the caller — replaces the baseline's internal `useCheckPermission(PERMISSIONS.secrets.create)` call. */
  canCreate?: boolean;
  createLabel?: string;
  /** Refresh the option list (e.g. after creating one out-of-band). Omit to hide the refresh action — replaces the baseline's internal RTK-Query `refetch`. */
  onRefresh?: () => void;
  /** Locks the field to whichever mode `value` currently implies — hides the toggle. */
  disableToggle?: boolean;
  tabLabels?: { secret?: string; password?: string };
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface SecretFieldProps {
  value: string;
  onChange: (value: string) => void;
  label: string;
  /** Used as both the underlying control's DOM `id` (for label association) and its HTML `name` attribute (for autofill/form submission) — the baseline kept these separate, unified here to stay in budget. */
  name?: string;
  required?: boolean;
  disabled?: boolean;
  error?: boolean;
  helperText?: string;
  /** Shows the show/hide toggle on the password field. Default `true`. */
  passwordVisibilityToggle?: boolean;
  /** Fires when the value transitions from masked to shown — e.g. for an audit-log call. Never fires on hide. */
  onReveal?: () => void;
  /** Fires on blur of the password field — a commit signal for callers that want to persist on blur rather than per keystroke (replaces the baseline's `onInputBlur`). */
  onSave?: () => void;
  secrets?: SecretFieldSecretsOptions;
}

// Printable ASCII only (space through tilde) — matches the baseline's
// `[^\x20-\x7E]` intent without the `\x` escapes (kept simple to read, and
// sidesteps any doubt about oxlint's `no-control-regex`-style rules, since
// neither boundary character is itself a control code).
const NON_PRINTABLE_ASCII_RE = /[^ -~]/g;

interface PasswordFieldProps {
  name: string | undefined;
  label: string;
  value: string;
  onChange: (event: ChangeEvent<HTMLInputElement>) => void;
  onBlur: (() => void) | undefined;
  disabled: boolean;
  required: boolean;
  error: boolean | undefined;
  helperText: string | undefined;
  showPassword: boolean;
  onToggleVisibility: (() => void) | undefined;
}

/** The masked/plain text entry, split out to keep `SecretField` under the §3.5 cyclomatic-complexity budget. This is the reveal/mask toggle target — see the component doc comment's mutation-proof note. */
function PasswordField({
  name,
  label,
  value,
  onChange,
  onBlur,
  disabled,
  required,
  error,
  helperText,
  showPassword,
  onToggleVisibility,
}: PasswordFieldProps): ReactNode {
  const toggleLabel = showPassword
    ? t('shared.ui.secretField.hideValue', 'Hide value')
    : t('shared.ui.secretField.showValue', 'Show value');

  return (
    <TextField
      variant="standard"
      fullWidth
      autoComplete="off"
      id={name}
      name={name}
      label={label}
      value={value}
      disabled={disabled}
      required={required}
      onChange={onChange}
      onBlur={onBlur}
      type={showPassword ? 'text' : 'password'}
      error={error}
      helperText={helperText}
      slotProps={{
        input: {
          endAdornment: onToggleVisibility && (
            <InputAdornment position="end">
              <IconButton
                aria-label={toggleLabel}
                edge="end"
                size="small"
                onClick={onToggleVisibility}
              >
                {showPassword ? <VisibilityOff fontSize="small" /> : <Visibility fontSize="small" />}
              </IconButton>
            </InputAdornment>
          ),
        },
      }}
    />
  );
}

/** Resolves a toggle tab's label — `secrets.tabLabels` override, else the `t()` default. Split out so `SecretField` doesn't carry the optional-chain lookups itself (§3.5 complexity budget). */
function resolveTabLabel(secrets: SecretFieldSecretsOptions | undefined, mode: SecretFieldMode): string {
  const custom = secrets?.tabLabels?.[mode];
  if (custom) return custom;
  return mode === 'secret'
    ? t('shared.ui.secretField.secretTab', 'Secret')
    : t('shared.ui.secretField.passwordTab', 'Password');
}

/**
 * A field that holds either a raw (masked) value or a reference to an
 * existing named secret, with a toggle between the two entry modes. Ported
 * from `apps/elitea-ui/src/[fsd]/shared/ui/secret-field/SecretField.jsx`.
 *
 * **Deliberate API deviation (this unit's central redesign task):** the
 * baseline reads `useSelector`/`useSecretsListQuery` (RTK-Query) directly
 * and calls `useCheckPermission(PERMISSIONS.secrets.create)` internally —
 * all Redux/entity/permissions coupling `shared/ui` cannot have (layer rule
 * R-L1). Redesigned as pure props/callbacks: `secrets.options` replaces the
 * query result, `secrets.onRefresh` replaces `refetch`, `secrets.canCreate`
 * replaces the internal permission check (the caller computes it), and
 * `secrets.onCreate` replaces the baseline's internal
 * `window.open(buildUrl(...))` (which itself depended on `RouteDefinitions`/
 * `getBasename()`/`useSelectedProjectId`/Redux `personal_project_id` — all
 * app-level). When `secrets` is omitted entirely, this renders as a plain
 * masked text field with no mode toggle, which is new relative to the
 * baseline (the baseline always rendered both modes) but a strict
 * generalisation: every baseline call site passed a `secrets`-shaped
 * config, so nothing existing loses capability.
 *
 * Other deviations:
 *  - `onReveal`/`onSave` are new, optional callbacks. `onSave` replaces the
 *    baseline's `onInputBlur` (`SecretManagementInput` wired `textFieldProps
 *    .onBlur`); `onReveal` is new — an audit-friendly hook a caller can use
 *    to log/gate an actual reveal, since real secret values may need a
 *    permission check the baseline's client-only `showPassword` toggle
 *    never had.
 *  - The baseline's custom `Toggle`/`SingleSelect` (with a `headerEnd`
 *    render prop for the refresh button living inside the dropdown) are
 *    replaced with plain MUI `ToggleButtonGroup`/`Select`, matching the
 *    precedent unit S7 set in `shared/ui/cron/`. The refresh action moves
 *    next to the select instead of inside its popup — a real interactive
 *    control nested in a `Select`'s adornment fights the popup's own click
 *    handling (a known MUI footgun) for no benefit here.
 *  - `tooltipDescription`'s label-becomes-tooltip layout switch is dropped
 *    for a single, simpler always-labelled layout; a caller wanting a
 *    description can render one beside this component.
 */
export function SecretField({
  value,
  onChange,
  label,
  name,
  required = true,
  disabled = false,
  error,
  helperText,
  passwordVisibilityToggle = true,
  onReveal,
  onSave,
  secrets,
}: SecretFieldProps): ReactNode {
  const [mode, setMode] = useState<SecretFieldMode>(() =>
    secrets && SECRET_REFERENCE_RE.test(value) ? 'secret' : 'password',
  );
  const [showPassword, setShowPassword] = useState(false);

  const handleToggleVisibility = useCallback(() => {
    setShowPassword((prev) => {
      const next = !prev;
      if (next) onReveal?.();
      return next;
    });
  }, [onReveal]);

  const handleModeChange = useCallback(
    (_event: MouseEvent<HTMLElement>, nextMode: SecretFieldMode | null): void => {
      if (!nextMode || nextMode === mode) return;
      setMode(nextMode);
      onChange('');
    },
    [mode, onChange],
  );

  const handlePasswordChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>): void => {
      onChange(event.target.value.replace(NON_PRINTABLE_ASCII_RE, ''));
    },
    [onChange],
  );

  const handleSecretChange = useCallback(
    (event: SelectChangeEvent<string>): void => {
      const next = event.target.value;
      if (next === CREATE_SECRET_VALUE) {
        secrets?.onCreate?.();
        return;
      }
      onChange(next);
    },
    [onChange, secrets],
  );

  const showToggle = secrets != null && secrets.disableToggle !== true;
  const secretTabLabel = resolveTabLabel(secrets, 'secret');
  const passwordTabLabel = resolveTabLabel(secrets, 'password');

  return (
    <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: (theme: Theme) => theme.spacing(1) }}>
      {mode === 'secret' && secrets ? (
        <SecretSelect
          name={name}
          label={label}
          value={value}
          onChange={handleSecretChange}
          secrets={secrets}
          disabled={disabled}
          required={required}
          error={error}
          helperText={helperText}
        />
      ) : (
        <PasswordField
          name={name}
          label={label}
          value={value}
          onChange={handlePasswordChange}
          onBlur={onSave}
          disabled={disabled}
          required={required}
          error={error}
          helperText={helperText}
          showPassword={showPassword}
          onToggleVisibility={passwordVisibilityToggle ? handleToggleVisibility : undefined}
        />
      )}
      {showToggle && (
        <ToggleButtonGroup
          exclusive
          size="small"
          value={mode}
          onChange={handleModeChange}
          disabled={disabled}
          aria-label={t('shared.ui.secretField.modeToggleLabel', 'Value type')}
        >
          <ToggleButton value="secret">{secretTabLabel}</ToggleButton>
          <ToggleButton value="password">{passwordTabLabel}</ToggleButton>
        </ToggleButtonGroup>
      )}
    </Box>
  );
}
