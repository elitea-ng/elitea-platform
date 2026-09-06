import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import { ToolTypes } from '@/entities/toolkit';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { ToolListError } from '@/shared/ui/ToolListError';

import type { RawToolkitTypeSchema } from '../../../lib/helpers/toolkitSchema.helpers';

import { ToolCustom } from '../ToolCustom';
import type { ToolCustomDetail } from '../ToolCustom';

import { FormViewToggle } from './FormViewToggle';
import { useToolkitFormState } from './ToolkitForm.hooks';
import type { ToolkitFormState } from './ToolkitForm.hooks';
import { resolveToolkitFormProps } from './ToolkitForm.types';
import type { ResolvedToolkitFormProps, ToolkitFormEditDetail, ToolkitFormProps } from './ToolkitForm.types';
import { ToolkitsOperationButtons } from './ToolkitsOperationButtons';

export type { ToolkitFormEditDetail, ToolkitFormProps, ToolkitValidationInjected } from './ToolkitForm.types';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * ToolkitForm/ToolkitForm.jsx` (597 lines) — the composition root that
 * picks `ToolJira`/`ToolConfluence`/`ToolBase` (sibling A4 sub-units) or
 * `ToolCustom` (this sub-unit, `../ToolCustom.tsx`) via
 * `../../../lib/helpers/toolComponent.helpers.ts`'s `getToolComponent`, when
 * EDITING a toolkit's own settings.
 *
 * NAME COLLISION, resolved per the mission brief's own warning: this is a
 * DIFFERENT component from `entities/toolkit`'s already-promoted
 * `ToolConfigurationForm` (ported from `pages/Applications/Components/
 * Tools/ToolConfigurationForm.jsx` — a tool-config renderer used from an
 * agent's tool list, not this one).
 *
 * SPLIT ACROSS FOUR FILES (this one plus `.types.ts`/`.helpers.ts`/
 * `.hooks.ts`) purely to stay under the §3.5 400-line-per-file /
 * complexity-12 budgets — the original single-file port was 660 lines with
 * a 42-complexity component function. `ToolkitForm.tsx` itself is now the
 * JSX-only render (`ToolkitFormView`) plus a thin composition wrapper;
 * every stateful hook lives in `ToolkitForm.hooks.ts`'s
 * `useToolkitFormState`, and every pure computation extracted from the
 * original body lives in `ToolkitForm.helpers.ts`. Re-exports every type a
 * real caller needs (`ToolkitEditorParts.tsx`/`ConfigurationTab.tsx`, this
 * slice's own `index.ts`) from this exact file path, so the split is
 * invisible to them.
 *
 * DISCLOSED REDESIGNS (grouped; each individual substitution mirrors an
 * established convention elsewhere in this session):
 *
 *  1. **No Formik, no Redux.** `useFormikContext()`'s `values`/
 *     `initialValues`/`setFieldValue`/`resetForm` and
 *     `useSelector(state => state.user)`'s `personal_project_id` all become
 *     explicit props (`formValues`/`formInitialValues`/`onSetFormField`/
 *     `onResetForm`/`personalProjectId`) — see `ApplicationEditForm.tsx`'s
 *     "No ambient form context" precedent. `useParams()`/`useSearchParams()`
 *     (toolkitType route param, `forceCustom` query flag) become
 *     `routeToolkitType`/`forceCustomView` props for the same reason (no
 *     app-level router context reachable from `features/`).
 *  2. **`useToolkitAvailableToolsQuery`/`useValidateToolkitQuery`.**
 *     CORRECTED (#440): this item used to say that no
 *     `toolkit_available_tools` endpoint existed. It does exist —
 *     `internal/api/router.go:1912` registers it, together with
 *     `toolkit_discover_tools` at :1914 — and `endpoints.manifest.json`
 *     now carries both as `toolkits.availableTools` /
 *     `toolkits.discoverTools`. The client for them is
 *     `entities/toolkit`'s `toolkitTools.useToolkitTools`, and
 *     `ui/test-tools/TestToolSettings.tsx` reads it for its tool picker.
 *
 *     The NAMES half of the baseline's `toolSchemaWithDynamicTools` is
 *     restored: `ToolkitForm.core.hooks.ts` reads the catalogue for a
 *     toolkit type that declares no tools of its own and writes the names
 *     into `selected_tools.items.enum`, which is where `ToolBase.render.tsx`'s
 *     `resolveAvailableTools` already looks. A failed read renders as its
 *     own state above the form with a retry, so an empty "Tools" section
 *     keeps its one meaning.
 *
 *     The ARGUMENT-SCHEMA half stays dropped, for a narrower reason: the
 *     route returns tool `id`/`name`/`type`/`description` and no argument
 *     schema (see `ui/test-tools/useGetSelectedToolSchema.ts`'s own header
 *     for the evidence), so there is nothing to enrich `args_schemas` WITH.
 *     Toolkit-settings validation errors are still accepted via an optional
 *     injected `toolkitValidation` prop (`{isError, error, refetch}`) — same
 *     "inject the network call" convention as
 *     `features/agents/api/useValidateToolkit.ts`.
 *  3. **`McpAuthHelpers.logout(url)`** (`features/mcp`, sideways-forbidden
 *     regardless) becomes an optional injected `onMcpScopesChanged`
 *     callback, fired in place of the baseline's direct logout call.
 *  4. **`useCreateConfiguration`/`useGetCurrentConfigurationAsSchemas`/
 *     `useGetConfigurationsListQuery`.** Real local ports (this slice's own
 *     `model/useCreateConfiguration.ts` — a duplicate of
 *     `features/agents/model/useCreateConfiguration.ts` per
 *     `no-sideways-features` — plus `model/useConfigurationsAsSchema.hooks.ts`
 *     and `api/useConfigurationsList.ts`), wired to REAL hand-registered
 *     `/configurations/*` endpoints (API-145/146/149/154 — confirmed
 *     against `endpoints.manifest.json`, unlike the toolkit-CRUD gaps
 *     above).
 *  5. **`handleGoBack`/`useToolkitView()`'s `setSaveActionParam`.** DROPPED
 *     entirely, not redesigned — the baseline's OWN `handleGoBack`
 *     (`ToolkitForm.jsx:82-113`) carries its own `//@todo: consider to
 *     remove this function and dependencies since we do not have GoBack
 *     functionality similar to old Tools` comment and is never actually
 *     called anywhere in the baseline file's JSX (grepped: the function is
 *     defined, never referenced). Honouring that TODO rather than porting
 *     genuinely-dead code (and the `useToolkitView()` read it alone would
 *     have needed) forward.
 *  6. **The `isLoading` gate.** See `ToolkitForm.helpers.ts`'s
 *     `resolveIsLoading` doc comment — a real, disclosed fix for a
 *     mount/refetch loop this sub-unit's own tests found against their
 *     `staleTime: 0` test harness; in real production (`staleTime: 30_000`,
 *     `app/providers/queryClient.ts`) the same gate causes at most a brief
 *     loading-spinner flash on a window-refocus refetch after 30+ seconds
 *     of inactivity, not a cascading remount loop.
 */

const containerSx: SxProps<Theme> = { maxWidth: '40.1875rem', margin: '0 auto' };
const formViewToggleSx: SxProps<Theme> = { marginBottom: '0.625rem' };
const loadingContainerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'center', width: '100%', height: '100%' };

/**
 * `view === ToolkitViewOptions.Json`/`forceCustomView` branch: renders
 * `ToolCustom` directly. Split out as its own tiny wrapper (rather than
 * inlined at the JSX call site) purely so `ToolCustomProps`'s specific
 * shape doesn't need reconciling against the generic `Record<string,
 * unknown>` `toolComponentProps` bag the dynamically-resolved
 * `ToolComponent` branch uses.
 */
function ToolCustomFallback({
  editToolDetail,
  setEditToolDetail,
  setToolErrors,
  schema,
  configurationSchema,
  editField,
  needToCheckSection,
  updateKey,
}: {
  readonly editToolDetail: ToolkitFormEditDetail;
  readonly setEditToolDetail: (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null) => void;
  readonly setToolErrors: (updater: (prev: Readonly<Record<string, boolean>>) => Record<string, boolean>) => void;
  readonly schema: RawToolkitTypeSchema | undefined;
  readonly configurationSchema: undefined;
  readonly editField: (field: string, value: unknown, replace?: boolean) => void | Promise<void>;
  readonly needToCheckSection: boolean;
  readonly updateKey: string | number | undefined;
}): ReactNode {
  const detail: ToolCustomDetail = { index: editToolDetail.index, id: editToolDetail.id, name: editToolDetail.name, description: editToolDetail.settings?.description as string | undefined, settings: editToolDetail.settings, type: editToolDetail.type };
  return (
    <ToolCustom
      key={updateKey}
      editToolDetail={detail}
      setEditToolDetail={(next) => setEditToolDetail((prev) => ({ ...prev, ...next }))}
      setToolErrors={setToolErrors}
      // `RawToolkitTypeSchema` (this file's own schema type) and
      // `ToolCustomSchema` (`../../lib/helpers/toolCustom.helpers.ts`, A4b)
      // describe the SAME underlying JSON-Schema-shaped object from two
      // different, independently-typed modules — structurally compatible
      // (`required?`/`properties?`, everything else covered by
      // `RawToolkitTypeSchema`'s index signature), so no cast is needed.
      schema={schema}
      configurationSchema={configurationSchema}
      editField={editField}
      needToCheckSection={needToCheckSection}
    />
  );
}

interface ToolkitFormViewProps {
  readonly props: ResolvedToolkitFormProps;
  readonly state: ToolkitFormState;
}

/** The JSX-only render — every value it reads is already resolved by `useToolkitFormState`/the caller's own prop defaults, so this stays a thin, low-complexity template. */
function ToolkitFormView({ props, state }: ToolkitFormViewProps): ReactNode {
  const { editToolDetail, onChangeToolDetail, isEditing, isToolDirty, hasNotSavedCredentials, isViewToggleVisible, hideOperationButtons, updateKey, sx, formValues, formInitialValues, onResetForm, isTeamProject, onSave, onSaveSuccess, onSaveError, onConfigurationCreated, projectId } = props;
  const { isLoading, view, setView, onManualViewChange, isValidSchema, effectiveToolSchema, toolListReadFailed, retryToolListRead, hasErrors, configuration, isCreatingConfiguration, isTestingConnection, onCreateConfiguration, onTestConnection, onRevertCredentials, setShowValidation, editField, setToolErrors, ToolComponent, toolComponentProps } = state;

  if (isLoading) {
    return (
      <Box sx={loadingContainerSx}>
        <CircularProgress />
      </Box>
    );
  }

  const showFormViewToggle = editToolDetail.type !== ToolTypes.custom.value && Boolean(effectiveToolSchema) && isViewToggleVisible;

  return (
    <Box sx={combineSx(containerSx, sx)}>
      {/* #440: a lost tool-list read is its own state. Without it the "Tools" section is empty, which is what a toolkit with no tools looks like. */}
      {toolListReadFailed && (
        <ToolListError
          onRetry={retryToolListRead}
          testId="toolkit-form-tool-list-error"
        />
      )}
      {showFormViewToggle && (
        <Box sx={formViewToggleSx}>
          <FormViewToggle
            view={view}
            onChangeView={onManualViewChange}
            disabled={!isValidSchema}
          />
        </Box>
      )}
      {!hideOperationButtons && (
        <ToolkitsOperationButtons
          isAdding={!isEditing}
          status={{ hasErrors, hasNotSavedToolConfiguration: hasNotSavedCredentials }}
          onCreateConfiguration={onCreateConfiguration}
          onTestConnection={onTestConnection}
          onRevertCredentials={onRevertCredentials}
          toolSchema={effectiveToolSchema}
          setShowValidation={setShowValidation}
          form={{ values: formValues, initialValues: formInitialValues, onReset: onResetForm ? () => onResetForm(formInitialValues ?? {}) : undefined }}
          isTeamProject={isTeamProject}
          save={{ onSave, onSuccess: onSaveSuccess, onError: onSaveError, onConfigurationCreated }}
          projectId={projectId}
          display={{ isDirty: isToolDirty, type: editToolDetail.type, configuration, isCreatingConfiguration, isTestingConnection, view, onChangeView: setView, hideViewToggle: !effectiveToolSchema }}
        />
      )}
      {ToolComponent ? (
        <ToolComponent
          key={updateKey}
          {...toolComponentProps}
        />
      ) : (
        <ToolCustomFallback
          editToolDetail={editToolDetail}
          setEditToolDetail={onChangeToolDetail}
          setToolErrors={setToolErrors}
          schema={effectiveToolSchema}
          configurationSchema={undefined}
          editField={editField}
          needToCheckSection={false}
          updateKey={updateKey}
        />
      )}
    </Box>
  );
}

export function ToolkitForm(rawProps: ToolkitFormProps): ReactNode {
  const props: ResolvedToolkitFormProps = resolveToolkitFormProps(rawProps);
  const state = useToolkitFormState(props);
  return (
    <ToolkitFormView
      props={props}
      state={state}
    />
  );
}
