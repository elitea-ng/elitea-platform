/**
 * pages/credentials/useCredentialFormController.ts — all state/mutation
 * orchestration for `CredentialForm.tsx`, split into its own file purely to
 * keep `CredentialForm`'s own render function under the §3.5
 * cyclomatic-complexity budget (≤12) — see that file's doc comment. Ported
 * behaviour from `apps/elitea-ui/src/pages/Credentials/CredentialForm.jsx`/
 * `hooks/credentials/{useCreateCredential,useUpdateCredential}.jsx`.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';

import {
  extractInformationFromCredentialError,
  SchemaField,
  useAvailableConfigurationsType,
  useConfigurationDetail,
  useCreateConfiguration,
  useDeleteConfiguration,
  useUpdateConfiguration,
} from '@/features/credentials';
import { t } from '@/shared/i18n';
import type { ConfigSchemaNode, ConfigurationTypeDescriptor } from '@/features/credentials';

import { useCredentialConnectionTest } from './useCredentialConnectionTest';
import { useCredentialDeleteGuard } from './useCredentialDeleteGuard';
import { useTypeSections } from './useTypeSections';

export interface CredentialFormContext {
  readonly projectId: string;
  readonly personalProjectId?: string;
  readonly isTeamProject: boolean;
  readonly canUpdate: boolean;
  readonly canDelete: boolean;
}

export interface CredentialFormMode {
  readonly kind: 'create' | 'edit';
  readonly credentialType?: string;
  readonly configId?: string;
  /** `/settings/create-configuration`/`/settings/edit-configuration` (ROUTE-063..065): different title, category selector hidden. */
  readonly configurationMode?: boolean;
}

export interface CredentialFormPrefill {
  readonly name?: string;
  readonly id?: string;
  readonly section?: string;
}

export interface CredentialFormControllerProps {
  readonly context: CredentialFormContext;
  readonly mode: CredentialFormMode;
  /**
   * `savedId` is the just-saved row's own id (`String(ConfigurationWire.id)`),
   * when the server's response carried one — `undefined` on any row that
   * somehow doesn't (never in practice for a real save). The
   * `/settings/*-configuration` routes forward it into the `reveal` search
   * param on return, so the section that gained/changed this row can open on
   * its own instead of the whole panel guessing (see `performSave`'s doc
   * comment).
   */
  readonly onSaved: (savedId?: string) => void;
  readonly onDiscarded: () => void;
  readonly prefill?: CredentialFormPrefill;
  readonly onTypeChosen?: (type: string) => void;
}

function findTypeDescriptor(list: readonly ConfigurationTypeDescriptor[] | undefined, type: string | undefined): ConfigurationTypeDescriptor | undefined {
  if (!type) return undefined;
  return list?.find((item) => item.type === type);
}

/**
 * `useCreateCredential.jsx`'s title fallback: `settings.elitea_title ||
 * \`${type}_${timestamp}\`` — same for update (`updated_configurartion_...`,
 * baseline's own typo not reproduced here since this port shares one title
 * builder for both paths; the value is a fallback default a user virtually
 * always overrides).
 *
 * `label` is a SECOND fallback this port adds ahead of the generated
 * timestamp, for the one case baseline's own `settings?.elitea_title`
 * genuinely has none to read: creating a brand-new credential, where no
 * stable key exists yet and the generic ToolBase form (out of this port's
 * scope, see `CredentialForm.tsx`'s disclosed-scope note) would otherwise
 * have slugified whatever the user just typed as the starting key. On
 * *edit*, `eliteaTitle` is seeded from the server and is basically never
 * empty in practice, so this fallback essentially never fires there —
 * `label` is deliberately still second (never first): a stable
 * non-empty `eliteaTitle` always wins, so an edit-save can never let a
 * rename creep into the lookup key other domains resolve this credential
 * by (finding A7-pages/1 — see `performSave`'s own `eliteaTitle` doc
 * comment).
 */
function buildTitle(eliteaTitle: string | undefined, label: string, type: string): string {
  if (eliteaTitle && eliteaTitle.trim() !== '') return eliteaTitle;
  if (label && label.trim() !== '') return label;
  return `${type}_${new Date().toISOString().slice(0, 19).replace(/[:-]/g, '')}`;
}

/** Maps a thrown `EliteaApiError` (see `shared/api/generated/mutator.ts`) onto the `{data:{message,field}}` shape `extractInformationFromCredentialError` expects. `unknown` in, since a caught value is never narrowed by TypeScript. */
function toCredentialApiError(error: unknown): { readonly data?: { readonly message?: unknown; readonly field?: unknown } } {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return {};
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null || !('body' in failure)) return {};
  const body = (failure as { body?: unknown }).body;
  if (typeof body !== 'object' || body === null) return {};
  const record = body as Record<string, unknown>;
  return { data: { message: record['error'] ?? record['message'], field: record['field'] } };
}

type SaveOutcome =
  | { readonly status: 'ok'; readonly id?: string }
  | { readonly status: 'fieldErrors'; readonly errors: Readonly<Record<string, string>> }
  | { readonly status: 'genericError' };

interface PerformSaveParams {
  readonly mode: CredentialFormMode;
  readonly projectId: string;
  readonly type: string;
  /** The freely-editable display name — always submitted as `label`, unmodified, exactly as typed (`hooks/credentials/useUpdateCredential.jsx`'s `getRequestBody`: `label: settings.label || ''`). */
  readonly label: string;
  /**
   * The internally-stable lookup key other domains resolve this credential
   * by. `undefined` on create (none exists yet — `buildTitle` derives one
   * from `label`, matching `hooks/credentials/useCreateCredential.jsx`'s
   * `settings?.elitea_title || \`${toolType}_${timestamp}\`` with no prior
   * value to fall back to). On edit this is the value loaded from the
   * server (seeded once by `useFormSeeding`, never re-derived from
   * `label`) — submitted as-is, matching `useUpdateCredential.jsx`'s own
   * `settings?.elitea_title || fallback-generated` (never `settings.label`).
   */
  readonly eliteaTitle: string | undefined;
  readonly shared: boolean;
  readonly data: Readonly<Record<string, unknown>>;
  readonly schemaProperties: Readonly<Record<string, ConfigSchemaNode>>;
  readonly createConfiguration: ReturnType<typeof useCreateConfiguration>;
  readonly updateConfiguration: ReturnType<typeof useUpdateConfiguration>;
}

/** The create-vs-update dispatch + error-mapping, isolated from the hook so its own complexity is measured separately (§3.5). */
async function performSave(params: PerformSaveParams): Promise<SaveOutcome> {
  const { mode, projectId, type, label, eliteaTitle, shared, data, schemaProperties, createConfiguration, updateConfiguration } = params;
  const body = { elitea_title: buildTitle(eliteaTitle, label, type), label, data, shared };
  try {
    // Captured on both branches (not just create): an edit-save's response
    // carries the same row's id, and the caller (`save` below) treats the two
    // uniformly — see `onSaved`'s doc comment on why the edit route also
    // wants this to reveal the row's section on return.
    const saved = mode.kind === 'edit' && mode.configId
      ? await updateConfiguration.mutateAsync({ projectId, configId: mode.configId, body })
      : await createConfiguration.mutateAsync({ projectId, body: { ...body, type } });
    return { status: 'ok', ...(saved.id !== undefined ? { id: String(saved.id) } : {}) };
  } catch (error) {
    const { newErrors } = extractInformationFromCredentialError({ error: toCredentialApiError(error), schemaProperties, settings: data });
    if (Object.keys(newErrors).length > 0) return { status: 'fieldErrors', errors: newErrors };
    return { status: 'genericError' };
  }
}

/** The saved row's id, or `undefined` on the create screen. A named function rather than an inline ternary in the hook body, for the same §3.5 complexity reason as `canSubmit` below. */
function editConfigId(mode: CredentialFormMode): string | undefined {
  return mode.kind === 'edit' ? mode.configId : undefined;
}

/** Split out of the hook body — one more condition there would push `useCredentialFormController` over the §3.5 complexity budget. */
function canSubmit(canUpdate: boolean, label: string, effectiveType: string | undefined, isSaving: boolean): boolean {
  return canUpdate && label.trim() !== '' && Boolean(effectiveType) && !isSaving;
}

interface FormSeedingSetters {
  readonly setName: Dispatch<SetStateAction<string>>;
  /**
   * The stable `elitea_title` lookup key, seeded once from the loaded
   * detail and never re-derived from `name`/`label` afterwards — see
   * `performSave`'s `eliteaTitle` doc comment. Bundled into this object
   * (rather than a 4th positional setter) purely to keep this function's
   * own parameter list from creeping every time a new field needs seeding.
   */
  readonly setEliteaTitle: Dispatch<SetStateAction<string | undefined>>;
  readonly setShared: Dispatch<SetStateAction<boolean>>;
  readonly setData: Dispatch<SetStateAction<Record<string, unknown>>>;
}

/**
 * Seeds `name`/`eliteaTitle`/`shared`/`data` once the relevant query
 * settles — split out of the hook body so the `useEffect` callback (a
 * separate function scope) carries its own branches, not the hook's.
 *
 * `name` (the single visible "Name" textbox this port's simplified form
 * exposes — see `CredentialForm.tsx`'s own disclosed-scope doc comment on
 * dropping the baseline's full ToolBase renderer) seeds from `label` FIRST
 * — `label` is the free-text display name a person actually reads/edits
 * (`pages/Credentials/EditCredential.jsx`'s `settings: { label:
 * configuration?.label || '' , elitea_title: … }` seeds them as two
 * genuinely independent fields) — falling back to `elitea_title` only for
 * legacy rows that somehow have no `label` at all, so the box is never
 * blank. `eliteaTitle` itself seeds separately and is never mixed into
 * `name`: it is carried untouched through to `performSave` regardless of
 * what the user later types into the Name box (fixes the blocker where
 * every edit-save — even a no-op one — silently overwrote the stored
 * `label` with the `elitea_title` value, and any rename rewrote the
 * stable `elitea_title` other domains resolve this credential by).
 */
function useFormSeeding(
  mode: CredentialFormMode,
  detailData: { elitea_title?: string; label?: string; shared?: boolean; data?: Readonly<Record<string, unknown>> } | undefined,
  dataSchema: ConfigSchemaNode | undefined,
  setters: FormSeedingSetters,
): void {
  const { setName, setEliteaTitle, setShared, setData } = setters;
  useEffect(() => {
    if (mode.kind === 'edit') {
      if (!detailData) return;
      setName(detailData.label ?? detailData.elitea_title ?? '');
      setEliteaTitle(detailData.elitea_title);
      setShared(detailData.shared ?? false);
      setData({ ...SchemaField.initialData(dataSchema), ...detailData.data });
      return;
    }
    if (dataSchema) setData(SchemaField.initialData(dataSchema));
    // eslint-disable-next-line react-hooks/exhaustive-deps -- re-seeds only when the loaded detail or the resolved type schema actually changes.
  }, [mode.kind, detailData, dataSchema]);
}

export function useCredentialFormController(props: CredentialFormControllerProps) {
  const { context, mode, onSaved, onDiscarded, prefill, onTypeChosen } = props;

  const [name, setName] = useState(prefill?.name ?? '');
  const [eliteaTitle, setEliteaTitle] = useState<string | undefined>(undefined);
  const [shared, setShared] = useState(false);
  const [data, setData] = useState<Record<string, unknown>>({});
  const [apiError, setApiError] = useState('');
  const [fieldErrors, setFieldErrors] = useState<Readonly<Record<string, string>>>({});

  const detail = useConfigurationDetail(context.projectId, mode.configId, { enabled: mode.kind === 'edit' });
  const typeQuery = useTypeSections(mode, prefill?.section, detail.data?.section, detail.data !== undefined);
  const availableTypes = useAvailableConfigurationsType(
    { section: typeQuery.sections },
    { enabled: typeQuery.enabled },
  );

  /**
   * On create, the chosen type comes from `mode.credentialType` — which the
   * route reads off the `:credentialType` URL segment — and NOT from local
   * state. Single source of truth, matching the baseline, where
   * `pages/Credentials/CreateCredential.jsx:24` reads `useParams()` and
   * `CredentialTypeSelector` selection NAVIGATES to that URL rather than
   * setting component state (`hooks/credentials/useCredentialSearch.js:29`).
   *
   * A local `selectedType` used to shadow this. It made the two disagree the
   * moment the URL changed underneath the mounted component: picking a type
   * left the URL on the parent, and pressing Back then dropped the param
   * while the form stayed on screen. The baseline needs an explicit
   * "clear the form when the param disappears" effect
   * (`CreateCredential.jsx:160`) precisely because it keeps a parallel copy;
   * deriving straight from the prop means there is nothing to resynchronise.
   */
  const effectiveType = mode.kind === 'edit' ? detail.data?.type : mode.credentialType;
  const typeDescriptor = findTypeDescriptor(availableTypes.data, effectiveType);
  // The wire schema nests the actual configurable fields one level down, at
  // `config_schema.properties.data.properties.<field>` — the top-level
  // `config_schema.properties` object has exactly one member, `data`
  // (verified against `CredentialTypeSelector.tsx`'s identical unwrap and
  // `useMultiSectionConfigurations.js`'s `config_schema.properties.data
  // .metadata.hidden` read in the baseline).
  // `?.` on `config_schema` as well: the field is required by the wire type
  // but not by the wire, and an entry without one must yield an empty form
  // rather than throw past the route's non-existent error boundary (#131).
  const dataSchema = typeDescriptor?.config_schema?.properties?.['data'];
  // Memoized (not `?? {}` inline): a fresh `{}` every render would defeat
  // `save`'s `useCallback` memoization below (react-hooks/exhaustive-deps
  // correctly flags a dependency that "changes every render" as a real bug,
  // not a style nit — `save` would never actually stay referentially stable).
  const schemaProperties = useMemo(() => dataSchema?.properties ?? {}, [dataSchema]);
  const schemaSections = useMemo(() => dataSchema?.metadata?.sections ?? {}, [dataSchema]);
  const schemaRequiredFields = useMemo(() => dataSchema?.required ?? [], [dataSchema]);

  useFormSeeding(mode, detail.data, dataSchema, { setName, setEliteaTitle, setShared, setData });

  // Delete guard (finding A7-pages/2) — see `./useCredentialDeleteGuard.ts`
  // for the full baseline citation, the reason this uses
  // `useConfigurationsList` rather than a dedicated
  // `getConfigurationsBySection` hook, and an out-of-scope follow-up this
  // guard's own reactivity surfaces in `CredentialsControls.tsx`.
  const deleteGuard = useCredentialDeleteGuard(mode.kind === 'edit', context.projectId, context.canDelete, detail.data?.section);

  const createConfiguration = useCreateConfiguration();
  const updateConfiguration = useUpdateConfiguration();
  const deleteConfiguration = useDeleteConfiguration();

  const isSaving = createConfiguration.isPending || updateConfiguration.isPending;
  const canSave = canSubmit(context.canUpdate, name, effectiveType, isSaving);

  // Collapses 4 separate `save` dependencies into 1
  // — keeps the `useCallback` dependency array within the §3.5 budget
  // (≤8 entries) without losing memoization correctness (this object is
  // itself re-derived only when one of the four fields actually changes).
  const formValues = useMemo(() => ({ name, eliteaTitle, shared, data }), [name, eliteaTitle, shared, data]);

  /**
   * Reports the pick upward and nothing else — the caller owns the URL, and
   * the URL owns `effectiveType` (see its doc comment above). A route that
   * forgets to pass `onTypeChosen` therefore has an inert picker, which is
   * the correct failure mode: it is visible immediately, unlike the silent
   * URL/state divergence the previous local-state version produced.
   */
  const chooseType = useCallback(
    (type: string) => {
      onTypeChosen?.(type);
    },
    [onTypeChosen],
  );

  /**
   * The connection test owns its own two-call decision — see
   * `./useCredentialConnectionTest.ts`. The one thing it needs from here is
   * to be told when a field is edited, so it can tell "the sealed secret the
   * server already holds" from "a new secret the user just typed"; that is
   * what `noteFieldChanged` below is threaded into `setField` for.
   */
  const connectionTest = useCredentialConnectionTest({
    projectId: context.projectId,
    configId: editConfigId(mode),
    configType: effectiveType,
    data,
    schemaProperties,
  });
  const { noteFieldChanged } = connectionTest;

  const setField = useCallback((fieldKey: string, value: unknown): void => {
    noteFieldChanged(fieldKey);
    setData((prev) => ({ ...prev, [fieldKey]: value }));
  }, [noteFieldChanged]);

  const save = useCallback(() => {
    if (!effectiveType) return;
    setApiError('');
    setFieldErrors({});
    void performSave({
      mode,
      projectId: context.projectId,
      type: effectiveType,
      label: formValues.name,
      eliteaTitle: formValues.eliteaTitle,
      shared: formValues.shared,
      data: formValues.data,
      schemaProperties,
      createConfiguration,
      updateConfiguration,
    }).then((outcome) => {
      if (outcome.status === 'ok') {
        onSaved(outcome.id);
      } else if (outcome.status === 'fieldErrors') {
        setFieldErrors(outcome.errors);
      } else {
        setApiError(t('credentials.form.saveFailed', 'Failed to save credential'));
      }
    });
  }, [effectiveType, mode, context.projectId, formValues, schemaProperties, createConfiguration, updateConfiguration, onSaved]);

  const remove = useCallback(() => {
    if (!mode.configId) return;
    deleteConfiguration.mutate({ projectId: context.projectId, configId: mode.configId }, { onSuccess: onDiscarded });
  }, [mode.configId, deleteConfiguration, context.projectId, onDiscarded]);

  return {
    availableTypes,
    effectiveType,
    typeDescriptor,
    schemaProperties,
    schemaSections,
    schemaRequiredFields,
    name,
    setName,
    shared,
    setShared,
    data,
    setField,
    apiError,
    fieldErrors,
    testResult: connectionTest.testResult,
    testMessage: connectionTest.testMessage,
    isSaving,
    isTesting: connectionTest.isTesting,
    isDeleting: deleteConfiguration.isPending,
    canSave,
    canDelete: deleteGuard.canDelete,
    deleteDisabledReason: deleteGuard.deleteDisabledReason,
    chooseType,
    save,
    testConnection: connectionTest.testConnection,
    remove,
  };
}
