/**
 * The composition root for the toolkit credential picker (#308).
 *
 * `features/toolkits` renders the toolkit form, but the picker itself is
 * `features/credentials`' `CredentialsSelect`. `no-sideways-features`
 * (`.dependency-cruiser.cjs`) forbids one `features/*` slice from importing
 * another, so the toolkit form can only declare a slot. `pages/` sits above
 * both and may import either, so this file is the one place in the app that can
 * put the real picker behind that slot — the same seam and the same reason as
 * `./sharepointAuthModals.tsx`.
 *
 * Until this file existed NOTHING filled the slot. `ToolBaseProperty.dispatch.tsx`'s
 * `renderCredentialLike` opens with `if (!ctx.slots?.renderCredentialLikeField
 * || …) return null`, so every `configuration`-kind field rendered as blank
 * space, and the index schedule modal's `renderCredentialsSelect` was threaded
 * through four components and supplied by no one but a test.
 *
 * TWO slots, ONE picker. The toolkit form and the schedule modal each declare
 * their own render-prop with its own prop shape, so this file exports one hook
 * per slot over a single shared component.
 *
 * DISCLOSED DEVIATION — "create new credential" NAVIGATES.
 * The baseline reveals the new credential's own fields INLINE inside the
 * toolkit form (`setShowConfigurableFields`, driving `ToolkitForm`'s existing
 * `onCreateConfiguration`/`isCreatingConfiguration` pair). The slot context
 * (`CredentialLikeFieldContext`) carries no handle for that, so this supplier
 * sends the user to the real create-credential route instead. The action is
 * real either way, and it is a deliberate user choice. Unsaved toolkit edits
 * are not preserved across it — `EditToolkit.tsx`'s own doc comment already
 * records that this page drops nav-blocking-while-dirty. Removing the create
 * rows instead was rejected: a project with no saved credential of the wanted
 * type would then get a picker with nothing in it at all.
 */
import { useCallback, useEffect, useMemo, type ReactNode } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { CredentialsSelect, useCredentialValidation } from '@/features/credentials';

import { useCredentialRows, type CredentialPickerRow } from './useCredentialRows';

/** The section every credential-kind property carries; `vectorstorage` is the other value the catalogue serves. */
const CREDENTIALS_SECTION = 'credentials';

/** Route `/_shell/credentials/create-credential/$credentialType` (`src/routes/_shell/credentials/create-credential.$credentialType.tsx`). */
const CREATE_CREDENTIAL_ROUTE = '/credentials/create-credential/$credentialType';

/** The toolkit settings shape for a saved credential reference: `{elitea_title, private}` (baseline `ToolBaseProperty.jsx`'s `settings[k]`). */
interface StoredCredentialValue {
  readonly elitea_title: string;
  readonly private: boolean;
}

function toSelectValue(value: unknown): { readonly eliteaTitle: string; readonly isPrivate: boolean } | null {
  if (typeof value !== 'object' || value === null) return null;
  const record = value as Record<string, unknown>;
  const title = record['elitea_title'];
  if (typeof title !== 'string' || title.trim() === '') return null;
  return { eliteaTitle: title, isPrivate: Boolean(record['private']) };
}

function toStoredValue(value: { readonly eliteaTitle: string; readonly isPrivate: boolean } | null): StoredCredentialValue | null {
  if (value === null) return null;
  return { elitea_title: value.eliteaTitle, private: value.isPrivate };
}

/** `CredentialsSelectFieldProps` declares its optional members WITHOUT `| undefined`, and `exactOptionalPropertyTypes` is on, so an absent value must be an absent KEY — not an explicit `undefined`. */
function toSelectField(field: CredentialPickerFieldProps): {
  readonly label: string;
  readonly required?: boolean;
  readonly error: boolean;
  readonly helperText?: string;
  readonly disabled: boolean;
} {
  return {
    label: field.label,
    ...(field.required === undefined ? {} : { required: field.required }),
    error: field.error,
    ...(field.helperText === undefined ? {} : { helperText: field.helperText }),
    disabled: field.disabled,
  };
}

/** Not exported: `credentialPickerSlots.tsx` builds this object inline and never names the type. An export with no importer fails the `knip --max-issues 0` gate. */
interface CredentialPickerFieldProps {
  readonly label: string;
  readonly required?: boolean | undefined;
  readonly error: boolean;
  readonly helperText: string | undefined;
  readonly disabled: boolean;
}

export interface ToolkitCredentialPickerProps {
  readonly projectId: string | undefined;
  /** `credentials` or `vectorstorage`, read off the property's resolved `$defs` entry. */
  readonly section: string;
  readonly configurationTypes: readonly string[];
  readonly value: unknown;
  readonly onChange: (value: unknown, options?: { readonly isAutoSelect: boolean }) => void;
  readonly field: CredentialPickerFieldProps;
  readonly onlyPublic?: boolean | undefined;
}

export function ToolkitCredentialPicker(props: ToolkitCredentialPickerProps): ReactNode {
  const { projectId, section, configurationTypes, value, onChange, field, onlyPublic = false } = props;
  const navigate = useNavigate();
  const { rows, hasFetchedData, isFetching, refresh } = useCredentialRows({ projectId, section, configurationTypes, onlyPublic });
  const validation = useCredentialValidation();

  const credentialType = configurationTypes[0] ?? '';
  useBatchValidation({ rows, hasFetchedData, section, projectId, validation });

  const state = useMemo(
    () => ({
      configurations: rows,
      hasFetchedData,
      isFetching,
      getStatus: validation.getCredentialStatus,
      getMessage: validation.getCredentialMessage,
    }),
    [rows, hasFetchedData, isFetching, validation.getCredentialStatus, validation.getCredentialMessage],
  );

  const openCreatePage = useCallback(() => {
    void navigate({ to: CREATE_CREDENTIAL_ROUTE, params: { credentialType } });
  }, [navigate, credentialType]);

  const revalidate = useRevalidate({ rows, projectId, validation });
  const resetStatuses = validation.resetStatuses;
  const handlers = useMemo(
    () => ({
      onSelect: (next: { readonly eliteaTitle: string; readonly isPrivate: boolean } | null, meta: { readonly isAutoSelect: boolean }) => {
        onChange(toStoredValue(next), meta);
      },
      onRefresh: () => {
        resetStatuses();
        refresh();
      },
      onCreate: openCreatePage,
      onRevalidate: revalidate,
    }),
    [onChange, resetStatuses, refresh, openCreatePage, revalidate],
  );

  return (
    <CredentialsSelect
      value={toSelectValue(value)}
      state={state}
      handlers={handlers}
      field={toSelectField(field)}
      type={credentialType}
      // The baseline's own gate: a vector-storage reference is picked, never created here.
      isCreationAllowed={section !== 'vectorstorage'}
      mismatch={{ mismatchedPrivateCredential: false, createHref: `/credentials/create-credential/${credentialType}` }}
    />
  );
}

interface BatchValidationParams {
  readonly rows: readonly CredentialPickerRow[];
  readonly hasFetchedData: boolean;
  readonly section: string;
  readonly projectId: string | undefined;
  readonly validation: ReturnType<typeof useCredentialValidation>;
}

/**
 * The baseline's own post-load batch test-connection pass
 * (`useCredentialsData.hooks.js`'s second effect), which is what turns a row's
 * "invalid credential" marker on. Only the `credentials` section is tested; a
 * vector-storage reference has no connection to check.
 *
 * THE SAVED-ROW ROUTE, NOT THE CANDIDATE-PAYLOAD ONE. This used to post
 * `row.data` to `POST /configurations/check_connection/…`, and `row.data` is
 * what the list endpoint returned — a `{{secret.NAME}}` reference where the
 * token used to be (`internal/api/v2/configurations/secret_sealing.go`). So the
 * provider was asked to authenticate a literal template string. For a toolkit
 * credential that answered "not supported yet" and the mistake was invisible;
 * now that toolkit types are really probed
 * (`internal/api/v2/configurations/toolkit_check.go`) it would mark every saved
 * credential in the project as refused, and keep marking a FIXED one refused
 * for ever. `check_stored_connections` reads the row and redeems the secret
 * server-side, which is the only form of this check that can ever come back
 * green.
 */
function useBatchValidation({ rows, hasFetchedData, section, projectId, validation }: BatchValidationParams): void {
  const batchValidateStoredCredentials = validation.batchValidateStoredCredentials;
  useEffect(() => {
    if (!hasFetchedData || section !== CREDENTIALS_SECTION || rows.length === 0 || projectId === undefined) return;
    void batchValidateStoredCredentials(
      rows.map((row) => ({
        projectId: row.ownerProjectId ?? projectId,
        // `useCredentialValidation` keys its statuses by `credentialId`, and
        // `CredentialsSelect` reads them back by `eliteaTitle`. The title IS
        // the key on both sides; using the row's uid here would key every
        // status under an id the select never asks for, and every row would
        // report `idle` for ever. The row id travels separately, as `configId`.
        credentialId: row.eliteaTitle,
        configId: row.id,
      })),
    );
  }, [hasFetchedData, section, projectId, rows, batchValidateStoredCredentials]);
}

interface RevalidateParams {
  readonly rows: readonly CredentialPickerRow[];
  readonly projectId: string | undefined;
  readonly validation: ReturnType<typeof useCredentialValidation>;
}

/**
 * The per-row "check again" action behind `CredentialOptionLabel`'s refresh
 * control.
 *
 * It is the half of the indicator a user acts on: fix the credential, press
 * Reload, and the marker clears without a page reload. That only works over the
 * SAVED-row route — see `useBatchValidation` above for why the candidate-payload
 * route can never answer this question about a stored row.
 */
function useRevalidate({ rows, projectId, validation }: RevalidateParams): (eliteaTitle: string) => void {
  const { resetStatus, validateStoredCredential } = validation;
  return useCallback(
    (eliteaTitle: string) => {
      const row = rows.find((candidate) => candidate.eliteaTitle === eliteaTitle);
      if (row === undefined || projectId === undefined) return;
      resetStatus(eliteaTitle);
      void validateStoredCredential({
        projectId: row.ownerProjectId ?? projectId,
        credentialId: eliteaTitle,
        configId: row.id,
      });
    },
    [rows, projectId, resetStatus, validateStoredCredential],
  );
}
