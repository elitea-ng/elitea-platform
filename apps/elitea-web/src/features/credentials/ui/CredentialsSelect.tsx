/**
 * ui/CredentialsSelect.tsx — a labelled dropdown for attaching a saved
 * credential (or creating a new one) to a toolkit/agent/model-config field.
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/credentials/ui/credentials-select/CredentialsSelect.jsx`.
 * Manifest COPY-113, ACT-041 (test-connection-via-onRevalidate).
 *
 * DISCLOSED REDESIGN, forced by real API differences in components this
 * unit does not own:
 *  - `shared/ui/SingleSelect` (unit S1-D) takes `label: string` only per
 *    option (see `SingleSelectMenuItem.tsx`) — deliberately trimmed, no
 *    rich-node option content, no grouped sections with sticky headers, no
 *    `headerEnd` popup slot (see that component's own doc comment). This
 *    component's rows need `CredentialOptionLabel`'s icon + inline actions,
 *    which a plain string cannot carry. Built directly on MUI
 *    `Select`/`MenuItem` instead — the SAME escape hatch
 *    `shared/ui/SecretField.tsx`'s own `SecretSelect` already uses for
 *    exactly this reason (rich per-option content a trimmed shared
 *    component can't express), not a new pattern invented here.
 *  - GA analytics (`useTrackEvent`) and the MCP-token silent-unlock check
 *    (`McpAuthHelpers.loadTokens()`) are dropped — both are cross-domain
 *    concerns (`shared/lib/constants/analytic.constants`, the `mcp`
 *    feature slice) this unit may not import into (R-L1: sibling
 *    `features/*` slices do not import each other).
 *  - There is no session/project store yet anywhere in this app (see this
 *    unit's final report) — `selectedProjectId`/`personal_project_id`/the
 *    "open in new tab" URL and "create" navigation are all caller-supplied
 *    instead of read from Redux/`RouteDefinitions`.
 *  - The `vectorstorage`-section auto-select-project-default special case
 *    is out of scope (that behaviour is the settings/AI-configuration
 *    domain, unit A9); only the `credentials`-section auto-select (first
 *    shared row when the value is blank) is ported, and only as the
 *    `autoSelectFirstShared` opt-in flag a caller can set.
 *
 * **STATUS: live since #308.** This component had zero call sites for a long
 * time, because the one prospective consumer (`features/toolkits`' toolkit
 * form and index schedule modal) may not import it (`no-sideways-features` —
 * R-L1) and both only declared render-prop slots. The composition root that
 * fills those slots is `pages/toolkits/lib/credentialPicker.tsx`; read that
 * file for the whole routing story.
 */
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react';

import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import FormHelperText from '@mui/material/FormHelperText';
import InputLabel from '@mui/material/InputLabel';
import Select, { type SelectChangeEvent } from '@mui/material/Select';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { BUTTON_VARIANTS, BaseBtn } from '@/shared/ui/BaseBtn';

import {
  decodeCreateActionValue,
  decodeSavedCredentialValue,
  encodeSavedCredentialValue,
  isBlankEliteaTitle,
} from '../lib/credentialSelectValue';

import { CredentialMismatchFooter } from './CredentialMismatchFooter';
import { CredentialNotFoundValue } from './CredentialNotFoundValue';
import { CredentialsSearchField } from './CredentialsSearchField';
import { buildSavedRowMenuItem, renderCreateMenuItems } from './credentialsSelectMenuItems';

export interface CredentialOptionRow {
  readonly eliteaTitle: string;
  readonly isPrivate: boolean;
  readonly displayLabel: string;
  readonly credentialUrl?: string;
  readonly shared?: boolean;
}

export interface CredentialsSelectValue {
  readonly eliteaTitle: string;
  readonly isPrivate: boolean;
}

export interface CredentialsSelectState {
  readonly configurations: readonly CredentialOptionRow[];
  readonly hasFetchedData: boolean;
  readonly isFetching: boolean;
  readonly getStatus: (eliteaTitle: string) => 'idle' | 'checking' | 'valid' | 'invalid' | 'unsupported';
  readonly getMessage: (eliteaTitle: string) => string;
}

export interface CredentialsSelectHandlers {
  readonly onSelect: (value: CredentialsSelectValue | null, meta: { isAutoSelect: boolean }) => void;
  readonly onRefresh: () => void;
  readonly onCreate: (isPrivate: boolean) => void;
  readonly onRevalidate: (eliteaTitle: string) => void;
}

export interface CredentialsSelectFieldProps {
  readonly label?: string;
  readonly required?: boolean;
  readonly error?: boolean;
  readonly helperText?: string;
  readonly disabled?: boolean;
}

export interface CredentialsSelectMismatchProps {
  readonly mismatchedPrivateCredential: boolean;
  readonly createHref: string;
}

export interface CredentialsSelectProps {
  readonly value: CredentialsSelectValue | null;
  readonly state: CredentialsSelectState;
  readonly handlers: CredentialsSelectHandlers;
  readonly field?: CredentialsSelectFieldProps;
  readonly type?: string;
  readonly isCreationAllowed?: boolean;
  readonly mismatch?: CredentialsSelectMismatchProps;
  /** Old-app `credentials`-section auto-select behaviour: select the first saved row once loaded, if `value` is still blank. */
  readonly autoSelectFirstShared?: boolean;
}

export function CredentialsSelect({
  value,
  state,
  handlers,
  field,
  type,
  isCreationAllowed = true,
  mismatch,
  autoSelectFirstShared = false,
}: CredentialsSelectProps): ReactNode {
  const { configurations, hasFetchedData, isFetching, getStatus, getMessage } = state;

  // #919/ELITEA-2534: a plain MUI `Select` with no way to narrow a long
  // SAVED CREDENTIALS list except scrolling. The search box lives in a
  // `ListSubheader` (not a `MenuItem`) at the top of the popup, the same
  // technique MUI's own grouped-Select demos use, so `Select`'s child walk
  // (which looks for `MenuItem`s only) treats it as inert — it filters the
  // rows below, nothing else.
  const [query, setQuery] = useState('');
  const filteredConfigurations = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === '') return configurations;
    return configurations.filter((row) => row.displayLabel.toLowerCase().includes(needle));
  }, [configurations, query]);

  const selectedRow = useMemo(
    () => configurations.find((row) => row.eliteaTitle === value?.eliteaTitle && row.isPrivate === value?.isPrivate),
    [configurations, value],
  );

  const hasAutoSelectedRef = useRef(false);
  useEffect(() => {
    autoSelectFirstSharedRow({ autoSelectFirstShared, hasFetchedData, hasAutoSelectedRef, value, configurations, onSelect: handlers.onSelect });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fires once per mount when data first arrives, matching the baseline's `hasAutoSelectedRef` guard.
  }, [autoSelectFirstShared, hasFetchedData, configurations]);

  const selectStringValue = useMemo(() => {
    if (!hasFetchedData) return '';
    if (selectedRow) return encodeSavedCredentialValue({ eliteaTitle: selectedRow.eliteaTitle, isPrivate: selectedRow.isPrivate });
    return '';
  }, [hasFetchedData, selectedRow]);

  const handleChange = useCallback(
    (event: SelectChangeEvent<string>) => {
      const nextValue = event.target.value;
      const created = decodeCreateActionValue(nextValue);
      if (created) {
        handlers.onCreate(created.isPrivate);
        return;
      }
      const saved = decodeSavedCredentialValue(nextValue);
      if (!saved) return;
      const isSame = value?.eliteaTitle === saved.eliteaTitle && value?.isPrivate === saved.private;
      handlers.onSelect(isSame ? null : { eliteaTitle: saved.eliteaTitle, isPrivate: saved.private }, { isAutoSelect: false });
    },
    [handlers, value],
  );

  const labelId = useId();
  const resolvedField = resolveFieldProps(field);
  const showMismatchFooter = isMismatchFooterVisible(value, selectedRow, hasFetchedData);

  return (
    <Box sx={containerSx}>
      <Box sx={fieldRowSx}>
        <FormControl
          variant="standard"
          fullWidth
          size="small"
          required={resolvedField.required}
          disabled={resolvedField.disabled}
          error={resolvedField.error}
        >
          {/* `id`/`labelId` pair up so the label becomes the select's ACCESSIBLE NAME. Without them MUI renders an unnamed `combobox`: the label is on screen, but no assistive technology can reach it, and a form with two credential references offers two indistinguishable controls. */}
          <InputLabel
            shrink
            id={labelId}
          >
            {resolvedField.label}
          </InputLabel>
          <Select<string>
            labelId={labelId}
            value={selectStringValue}
            onChange={handleChange}
            displayEmpty
            renderValue={() => renderSelectedValue(selectedRow, value, hasFetchedData)}
          >
            {isCreationAllowed && renderCreateMenuItems(type)}
            {configurations.length > 0 && (
              <CredentialsSearchField
                query={query}
                onQueryChange={setQuery}
              />
            )}
            {filteredConfigurations.map((row) =>
              buildSavedRowMenuItem({
                row,
                value,
                status: getStatus(row.eliteaTitle),
                message: getMessage(row.eliteaTitle),
                onSelect: handlers.onSelect,
                onRevalidate: handlers.onRevalidate,
              }),
            )}
          </Select>
          {resolvedField.helperText !== undefined && <FormHelperText>{resolvedField.helperText}</FormHelperText>}
        </FormControl>
        <Tooltip
          title={t('credentials.select.refresh', 'Refresh the configurations')}
          placement="top"
        >
          <BaseBtn
            data-testid="credentials-select-refresh"
            variant={BUTTON_VARIANTS.secondary}
            size="small"
            disabled={isFetching}
            onClick={handlers.onRefresh}
          >
            <RefreshIcon />
          </BaseBtn>
        </Tooltip>
      </Box>
      {showMismatchFooter && mismatch && renderMismatchFooter(mismatch, value, type)}
    </Box>
  );
}

interface AutoSelectFirstSharedParams {
  readonly autoSelectFirstShared: boolean;
  readonly hasFetchedData: boolean;
  readonly hasAutoSelectedRef: RefObject<boolean>;
  readonly value: CredentialsSelectValue | null;
  readonly configurations: readonly CredentialOptionRow[];
  readonly onSelect: CredentialsSelectHandlers['onSelect'];
}

/** The old-app `credentials`-section auto-select-first-row effect, split into its own function (§3.5 complexity budget) — see `CredentialsSelectProps.autoSelectFirstShared`'s doc comment. */
function autoSelectFirstSharedRow(params: AutoSelectFirstSharedParams): void {
  const { autoSelectFirstShared, hasFetchedData, hasAutoSelectedRef, value, configurations, onSelect } = params;
  if (!autoSelectFirstShared || !hasFetchedData || hasAutoSelectedRef.current) return;
  if (value && !isBlankEliteaTitle(value.eliteaTitle)) return;
  const first = configurations[0];
  if (!first) return;
  hasAutoSelectedRef.current = true;
  onSelect({ eliteaTitle: first.eliteaTitle, isPrivate: first.isPrivate }, { isAutoSelect: true });
}

interface ResolvedFieldProps {
  readonly label: string;
  readonly required: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly error: boolean | undefined;
  readonly helperText: string | undefined;
}

/** Resolves the optional `field` prop bundle to concrete values up front, so the render body reads plain fields instead of repeated `?.`/`??` chains (§3.5 complexity budget). */
function resolveFieldProps(field: CredentialsSelectFieldProps | undefined): ResolvedFieldProps {
  return {
    label: field?.label ?? t('credentials.select.defaultLabel', 'Credentials'),
    required: field?.required,
    disabled: field?.disabled,
    error: field?.error,
    helperText: field?.helperText,
  };
}

/** True once a non-blank value matches no loaded row (§3.5 complexity budget — kept out of the render body). */
function isMismatchFooterVisible(value: CredentialsSelectValue | null, selectedRow: CredentialOptionRow | undefined, hasFetchedData: boolean): boolean {
  return Boolean(value && !isBlankEliteaTitle(value.eliteaTitle) && !selectedRow && hasFetchedData);
}

function renderMismatchFooter(mismatch: CredentialsSelectMismatchProps, value: CredentialsSelectValue | null, type: string | undefined): ReactNode {
  return (
    <CredentialMismatchFooter
      mismatchedPrivateCredential={mismatch.mismatchedPrivateCredential}
      {...(value?.eliteaTitle !== undefined ? { credentialId: value.eliteaTitle } : {})}
      {...(type !== undefined ? { credentialType: type } : {})}
      createHref={mismatch.createHref}
    />
  );
}

function renderSelectedValue(
  selectedRow: CredentialOptionRow | undefined,
  value: CredentialsSelectValue | null,
  hasFetchedData: boolean,
): ReactNode {
  if (selectedRow) return selectedRow.displayLabel;
  if (value && !isBlankEliteaTitle(value.eliteaTitle)) {
    return (
      <CredentialNotFoundValue
        eliteaTitle={value.eliteaTitle}
        isPrivate={value.isPrivate}
        hasFetchedData={hasFetchedData}
      />
    );
  }
  return null;
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({ marginTop: theme.spacing(1), display: 'flex', flexDirection: 'column', gap: theme.spacing(1) });
const fieldRowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'flex-end', gap: theme.spacing(1) });
