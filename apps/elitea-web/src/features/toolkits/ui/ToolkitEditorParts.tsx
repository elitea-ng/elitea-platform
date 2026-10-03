import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';

import type { ToolkitWriteResult, UseToolkitCreateMutation, UseToolkitEditMutation } from '../api/toolkits';
import { useToolkitDetail } from '../api/toolkits';
import { normalizeLegacyOpenApiToolkit } from '../lib/helpers/legacyOpenApiMigration.helpers';
import { useGetCurrentToolkitSchemas } from '../lib/hooks/useGetCurrentToolkitSchemas.hooks';
import { useSelectedProjectId } from '../lib/hooks/useSelectedProjectId';
import { CreateToolkitButton } from './CreateToolkitButton';
import { SaveToolkitButton } from './SaveToolkitButton';
import type { ToolkitFormValues, ToolSchemaLike } from './SaveToolkitButton';
import { ToolkitTypeSelector } from './ToolkitTypeSelector';
import { ToolkitForm, type ToolkitFormEditDetail, type ToolkitValidationInjected } from './form/ToolkitForm/ToolkitForm';

/**
 * `ToolkitEditor.tsx`'s own supporting pieces (prop-shape types, state
 * hooks, body/save-button sub-components) — split out purely to keep
 * `ToolkitEditor.tsx` under the §3.5 400-line budget, same
 * `PipelineEditorParts.tsx` precedent this batch already established for
 * the sibling pipelines editor. The prop-shape types below live HERE
 * (rather than in `ToolkitEditor.tsx`, re-imported back into this file) so
 * the dependency edge between the two files stays one-directional
 * (`ToolkitEditor.tsx` -> `ToolkitEditorParts.tsx` only) — `PipelineEditor.tsx`
 * imports `PipelineEditorDeps`/`PipelineEditorShellProps` FROM
 * `PipelineEditorParts.tsx` for the exact same `no-circular` (R-L2) reason;
 * the reverse import direction this file used to have
 * (`import type {...} from './ToolkitEditor'`) created a real
 * `ToolkitEditor.tsx` <-> `ToolkitEditorParts.tsx` cycle, caught by
 * `npx depcruise --config .dependency-cruiser.cjs`.
 */

export interface ToolkitEditorShellProps {
  readonly isVisible: boolean;
  readonly isDirty: boolean;
  readonly onClose: () => void;
  readonly title: string;
  readonly onDiscard: () => void;
  readonly error: unknown;
  readonly saveButton: ReactNode;
  readonly contentSx?: SxProps<Theme>;
  readonly children: ReactNode;
}

export interface ToolkitEditorDeps {
  /** The chat-owned editor chrome (baseline: `pages/NewChat/components/BaseEditor.jsx`) — see `ToolkitEditor.tsx`'s module doc comment. */
  readonly renderShell: (props: ToolkitEditorShellProps) => ReactNode;
  /** No generated create endpoint exists yet — see `ToolkitEditor.tsx`'s module doc comment. */
  readonly createToolkit: UseToolkitCreateMutation;
  /** No generated edit endpoint exists yet — see `ToolkitEditor.tsx`'s module doc comment. */
  readonly saveToolkit: UseToolkitEditMutation;
  /** Credential-change warning gate — see `ToolkitEditor.tsx`'s module doc comment. Omitted entirely skips the check. */
  readonly checkBeforeSave?: (performSave: () => void) => boolean;
  /**
   * [R3] `usePublicProjectAccessCheck()` injection point — baseline:
   * `disabled={isPublic && !hasPublicProjectAccess}` (`ToolkitEditor.jsx:303`):
   * a public-project toolkit is only locked for users who LACK explicit
   * public-project access. No `features/project` slice exists to compute
   * this locally (same disclosed gap `AgentEditorPanel.derive.ts`/
   * `projectOptions.ts` independently give). Omitted (every caller today),
   * `disabled` reduces to `isPublic` alone — conservative, never LESS
   * locked than baseline (see `resolveToolkitFormDisabled`). A future
   * caller with a real permission source supplies it here, same
   * "inject the capability" convention as `checkBeforeSave` above.
   */
  readonly hasPublicProjectAccess?: boolean;
  readonly onEditorClosed?: () => void;
}

export interface ToolkitEditorParticipant {
  readonly isCreating?: boolean;
  readonly isMCP?: boolean;
  readonly id?: string | number;
  readonly entity_meta?: { readonly id?: string | number; readonly project_id?: string | number; readonly name?: string | undefined };
  readonly meta?: { readonly id?: string | number; readonly mcp?: boolean; readonly name?: string | undefined };
  readonly name?: string;
}

export interface ToolkitEditorProps {
  readonly toolkit: ToolkitEditorParticipant | null | undefined;
  readonly isVisible: boolean;
  readonly onCloseToolkitEditor?: () => void;
  readonly onToolkitCreated?: (result: ToolkitWriteResult) => void;
  readonly onToolkitUpdated?: (updatedParticipant: ToolkitEditorParticipant, isUpdate: true) => void;
  readonly deps: ToolkitEditorDeps;
}

function getToolkitId(toolkit: ToolkitEditorParticipant | null | undefined): string | undefined {
  const id = toolkit?.entity_meta?.id ?? toolkit?.id ?? toolkit?.meta?.id;
  return id === undefined ? undefined : String(id);
}

function resolveIsPublic(entityProjectId: string | number | undefined): boolean {
  if (entityProjectId === undefined) return false;
  const config = getConfig();
  if (config.status !== 'ok') return false;
  return isPublicProject(entityProjectId, config.config.vite_public_project_id);
}

/**
 * [R3] Baseline: `ToolkitEditor.jsx:303`'s `disabled={isPublic &&
 * !hasPublicProjectAccess}`. `hasPublicProjectAccess` is `undefined` (i.e.
 * `false`) for every caller today — see `ToolkitEditorDeps.
 * hasPublicProjectAccess`'s own doc comment — so this reduces to `isPublic`
 * alone until a real caller supplies `true`. Exported as a standalone pure
 * function (not inlined at its one call site) so the formula is
 * independently unit-tested.
 */
export function resolveToolkitFormDisabled(isPublic: boolean, hasPublicProjectAccess: boolean | undefined): boolean {
  return isPublic && !(hasPublicProjectAccess ?? false);
}

export function resolveToolkitName(isCreating: boolean, isMCP: boolean, editToolDetail: ToolkitFormEditDetail | null, toolkit: ToolkitEditorParticipant): string {
  if (isCreating) return t('toolkits.toolkitEditor.newTitle', 'New {{kind}}', { kind: isMCP ? 'MCP' : 'Toolkit' });
  return editToolDetail?.name ?? toolkit.meta?.name ?? toolkit.name ?? t('toolkits.toolkitEditor.title', 'Toolkit');
}

/** `editToolDetail`'s shape adapted for `ToolkitForm`'s `settings`/`name`-mutating `onChangeToolDetail`. */
function useEditorFormState(isCreating: boolean, toolkitDetails: ToolkitFormEditDetail | undefined, isVisible: boolean) {
  const [editToolDetail, setEditToolDetail] = useState<ToolkitFormEditDetail | null>(null);
  const [formInitialValues, setFormInitialValues] = useState<Readonly<Record<string, unknown>>>({ type: '' });

  useEffect(() => {
    if (isCreating) {
      setEditToolDetail(null);
    } else if (toolkitDetails && isVisible) {
      setEditToolDetail(normalizeLegacyOpenApiToolkit(toolkitDetails));
    }
  }, [toolkitDetails, isVisible, isCreating]);

  return { editToolDetail, setEditToolDetail, formInitialValues, setFormInitialValues };
}

/**
 * `ToolkitForm`'s `onSave` is a required prop, but this component always
 * renders it with `hideOperationButtons` — its own internal save button is
 * never shown, so this network-shaped prop is structurally unreachable
 * here. A stable no-op keeps the required prop satisfied without inventing
 * a fake network call (the REAL save path is `deps.createToolkit`/
 * `deps.saveToolkit`, wired into `CreateToolkitButton`/`SaveToolkitButton`
 * below instead).
 */
function unreachableToolkitFormSave(): Promise<Readonly<Record<string, unknown>>> {
  return Promise.resolve({});
}

export interface ToolkitEditorBodyProps {
  readonly isCreating: boolean;
  readonly isMCP: boolean;
  readonly editToolDetail: ToolkitFormEditDetail | null;
  /**
   * [R1] Already wrapped to mark the editor dirty on every real edit — see
   * `useToolkitEditorState`'s own `handleChangeToolDetail`. The optional
   * `options` argument mirrors `ToolkitForm`'s `onChangeToolDetail`
   * contract: `{ isAutoSelect: true }` (a child selector auto-picking a
   * fallback value, e.g. a shared credential default) must NOT flip the
   * dirty flag — see `handleChangeToolDetail`'s own inline comment.
   */
  readonly onChangeToolDetail: (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null, options?: { readonly isAutoSelect?: boolean }) => void;
  readonly formInitialValues: Readonly<Record<string, unknown>>;
  readonly setFormInitialValues: (updater: (prev: Readonly<Record<string, unknown>>) => Readonly<Record<string, unknown>>) => void;
  readonly disabled: boolean;
  readonly projectId: string | undefined;
  readonly revertCredentialsRef: { current: (() => void) | undefined };
  readonly onValidationStateChange: (state: { readonly hasErrors: boolean; readonly triggerValidation: () => void }) => void;
  /** #613 — the server's per-field save refusal, produced by `ToolkitEditor`'s `useToolkitSaveValidation` from the `CreateToolkitButton`/`SaveToolkitButton` rejection below. */
  readonly toolkitValidation: ToolkitValidationInjected;
}

export function ToolkitEditorBody({
  isCreating,
  isMCP,
  editToolDetail,
  onChangeToolDetail,
  formInitialValues,
  setFormInitialValues,
  disabled,
  projectId,
  revertCredentialsRef,
  onValidationStateChange,
  toolkitValidation,
}: ToolkitEditorBodyProps): ReactNode {
  if (!editToolDetail) {
    if (!isCreating) {
      return (
        <Box sx={centeredSx}>
          <Typography variant="bodyMedium" component="p" color="text.secondary">
            {t('toolkits.toolkitEditor.loading', 'Loading toolkit configuration...')}
          </Typography>
        </Box>
      );
    }
    return (
      <ToolkitTypeSelector
        onSelectTool={(value) => onChangeToolDetail(() => value)}
        setFormikInitialValues={setFormInitialValues}
        isMCP={isMCP}
        disableNavigation
      />
    );
  }

  return (
    <ToolkitForm
      editToolDetail={editToolDetail}
      onChangeToolDetail={onChangeToolDetail}
      isEditing={!isCreating}
      isViewToggleVisible={false}
      showNameFieldForcedly={false}
      showToolkitIcon={false}
      showOnlyConfigurationFields={false}
      // Create mode: never hidden (baseline: `isMCP || isCreating ? false :
      // true`, always `false` while `isCreating` is true). Edit mode:
      // baseline literal `false`, plus `hideNameInput={!isMCP}`.
      hideNameDescriptionInput={false}
      hideNameInput={!isCreating && !isMCP}
      hideOperationButtons
      isMCP={isMCP}
      disabled={disabled}
      projectId={projectId}
      formValues={formInitialValues}
      formInitialValues={formInitialValues}
      onValidationStateChange={onValidationStateChange}
      revertCredentialsRef={revertCredentialsRef}
      toolkitValidation={toolkitValidation}
      onSave={unreachableToolkitFormSave}
    />
  );
}

type ValidationState = { readonly hasErrors: boolean; readonly triggerValidation: () => void };

export interface ToolkitEditorSaveButtonProps {
  readonly isCreating: boolean;
  readonly editToolDetail: ToolkitFormEditDetail | null;
  readonly toolkitId: string | undefined;
  readonly isDirty: boolean;
  readonly validationState: ValidationState;
  readonly createProjectId: string | undefined;
  readonly saveProjectId: string | undefined;
  readonly deps: ToolkitEditorDeps;
  readonly onToolkitCreated: (result: ToolkitWriteResult) => void;
  readonly onToolkitSaved: (result: ToolkitWriteResult, toolkitData: ToolkitFormValues) => void;
}

export function ToolkitEditorSaveButton({
  isCreating,
  editToolDetail,
  toolkitId,
  isDirty,
  validationState,
  createProjectId,
  saveProjectId,
  deps,
  onToolkitCreated,
  onToolkitSaved,
}: ToolkitEditorSaveButtonProps): ReactNode {
  const toolSchema = editToolDetail?.schema as ToolSchemaLike | undefined;

  if (isCreating) {
    return (
      <CreateToolkitButton
        toolSchema={toolSchema}
        values={{ ...editToolDetail }}
        isDirty={isDirty}
        hasErrors={validationState.hasErrors}
        triggerValidation={validationState.triggerValidation}
        projectId={createProjectId}
        createToolkit={deps.createToolkit}
        onToolkitCreated={onToolkitCreated}
      />
    );
  }

  return (
    <SaveToolkitButton
      toolSchema={toolSchema}
      values={{ ...editToolDetail, id: toolkitId } as ToolkitFormValues}
      isDirty={isDirty}
      hasErrors={validationState.hasErrors}
      triggerValidation={validationState.triggerValidation}
      projectId={saveProjectId}
      saveToolkit={deps.saveToolkit}
      onToolkitSaved={onToolkitSaved}
      onBeforeSave={deps.checkBeforeSave}
    />
  );
}

function noopTriggerValidation(): void {}

export interface UseToolkitEditorStateResult {
  readonly isDirty: boolean;
  readonly setIsDirty: (value: boolean) => void;
  readonly validationState: ValidationState;
  readonly setValidationState: (value: ValidationState) => void;
  readonly revertCredentialsRef: { current: (() => void) | undefined };
  readonly isCreating: boolean;
  readonly isMCP: boolean;
  readonly projectId: string | undefined;
  readonly toolkitId: string | undefined;
  readonly scopedProjectId: string | undefined;
  readonly isPublic: boolean;
  readonly isError: boolean;
  readonly editToolDetail: ToolkitFormEditDetail | null;
  readonly formInitialValues: Readonly<Record<string, unknown>>;
  readonly setFormInitialValues: (updater: (prev: Readonly<Record<string, unknown>>) => Readonly<Record<string, unknown>>) => void;
  readonly handleChangeToolDetail: (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null, options?: { readonly isAutoSelect?: boolean }) => void;
  readonly handleDiscard: () => void;
}

/** Everything that needs to be recomputed once `toolkit` is known non-null. */
export function useToolkitEditorState(toolkit: ToolkitEditorParticipant, isVisible: boolean): UseToolkitEditorStateResult {
  const [isDirty, setIsDirty] = useState(false);
  const [validationState, setValidationState] = useState<ValidationState>({ hasErrors: false, triggerValidation: noopTriggerValidation });
  const revertCredentialsRef = useRef<(() => void) | undefined>(undefined);

  const isCreating = toolkit.isCreating ?? false;
  const isMCP = toolkit.isMCP ?? toolkit.meta?.mcp ?? false;

  const projectId = useSelectedProjectId();
  const toolkitId = getToolkitId(toolkit);
  const entityProjectId = toolkit.entity_meta?.project_id;
  const scopedProjectId = entityProjectId !== undefined ? String(entityProjectId) : projectId;
  const isPublic = resolveIsPublic(entityProjectId);

  // Ensures toolkit-type schemas are loaded (used by `ToolkitTypeSelector`/
  // `ToolkitForm` internally) — baseline: `ToolkitEditor.jsx:107`.
  useGetCurrentToolkitSchemas();

  const { detail: toolkitDetails, isError } = useToolkitDetail({
    projectId: scopedProjectId,
    toolkitId: isVisible && !isCreating ? toolkitId : undefined,
  });
  const typedDetails = toolkitDetails;

  const { editToolDetail, setEditToolDetail, formInitialValues, setFormInitialValues } = useEditorFormState(isCreating, typedDetails, isVisible);

  const handleChangeToolDetail = useCallback(
    (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null, options?: { readonly isAutoSelect?: boolean }) => {
      // An auto-corrected field (a child selector picking a fallback value
      // on the user's behalf, e.g. a shared credential/embedding-model
      // default) must not mark the editor dirty — baseline:
      // `pages/Toolkits/ConfigurationTab.jsx`'s own `onChangeToolDetail`
      // (`if (!options?.isAutoSelect) setIsToolDirty(...)`); see this
      // prop's own doc comment (`ToolkitEditorBodyProps.onChangeToolDetail`)
      // for the fuller cross-reference.
      if (!options?.isAutoSelect) {
        setIsDirty(true);
      }
      setEditToolDetail(updater);
    },
    [setEditToolDetail],
  );

  const handleDiscard = useCallback(() => {
    if (isCreating) {
      setEditToolDetail(() => null);
      setFormInitialValues(() => ({ type: '' }));
    } else if (typedDetails) {
      setEditToolDetail(() => normalizeLegacyOpenApiToolkit(typedDetails));
    }
    setIsDirty(false);
  }, [isCreating, typedDetails, setEditToolDetail, setFormInitialValues]);

  return {
    isDirty,
    setIsDirty,
    validationState,
    setValidationState,
    revertCredentialsRef,
    isCreating,
    isMCP,
    projectId,
    toolkitId,
    scopedProjectId,
    isPublic,
    isError,
    editToolDetail,
    formInitialValues,
    setFormInitialValues,
    handleChangeToolDetail,
    handleDiscard,
  };
}

export const EMPTY_PARTICIPANT: ToolkitEditorParticipant = {};

const centeredSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', alignItems: 'center', height: '12.5rem' };
export const emptyContentSx: SxProps<Theme> = { padding: 0 };
