import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import { ToolEvents } from '@/entities/toolkit';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { eventEmitter } from '@/shared/lib/eventEmitter';
import { toolkitFormErrorMessage } from '../../../lib/errorMessage';
import { useCredentialWarning } from '../../../model/useCredentialWarning.hooks';
import type { RevertedCredentialDetail } from '../../../model/credentialWarning.helpers';

import type { ToolkitsOperationButtonsProps } from './ToolkitsOperationButtons.types';

/**
 * `formValues`/`formInitialValues` arrive as plain, wide `Record<string,
 * unknown>` bags (the outer form's whole values object — see the module doc
 * comment's redesign 1) — narrowed to `useCredentialWarning`'s specific
 * `RevertedCredentialDetail`/`CredentialWarningDetail` shapes (both of which
 * require a typed, non-`unknown` `settings?:` field) only at that one call
 * site below, via this shared cast helper, rather than widening
 * `useCredentialWarning`'s own parameter types for one caller.
 */
function asCredentialWarningDetail(value: Readonly<Record<string, unknown>> | undefined): RevertedCredentialDetail | undefined {
  return value;
}

import { CredentialWarningModal } from './CredentialWarningModal';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * ToolkitForm/ToolkitsOperationButtons.jsx` (332 lines) — Save/Update/
 * Discard for the toolkit-settings form, driven by `entities/toolkit`'s
 * `ToolEvents` pub/sub (`../../../lib/eventEmitter.ts`).
 *
 * DISCLOSED REDESIGNS (each matches an established convention elsewhere in
 * this session, cited at its own site below):
 *  1. **No Formik.** `useFormikContext()`'s `values`/`initialValues`/
 *     `resetForm`/`setValues` become explicit props (`formValues`/
 *     `formInitialValues`/`onResetForm`/`onSaveSuccess` — see
 *     `ApplicationEditForm.tsx`'s own "No ambient form context" precedent).
 *  2. **No `useToolkitEditMutation` (RTK Query).** No generated OR
 *     hand-registered endpoint exists for editing a toolkit anywhere in
 *     this worktree (grepped `endpoints.manifest.json`/`shared/api/
 *     generated/toolkits/toolkits.ts`: only `listToolkits`/
 *     `listToolkitInstances`) — a REAL, disclosed backend gap, not a
 *     porting shortcut (matches the mission brief's own pre-disclosed
 *     "No generated endpoint exists for toolkit validation" finding, one
 *     level over: the EDIT endpoint is missing too). The save call is
 *     dependency-injected via the required `onSave` prop, exactly the
 *     `entities/application-form/ui/ApplicationValidator.tsx`/
 *     `features/agents/api/useValidateToolkit.ts` "inject the actual
 *     network call, own the orchestration" convention.
 *  3. **No Redux `dispatch(eliteaApi.util.updateQueryData(...))`.** This
 *     app has no RTK-Query cache to patch; `onSave`'s own TanStack Query
 *     mutation (once a real endpoint exists) owns its own cache
 *     invalidation — this component only calls it and reacts to the
 *     result.
 *  4. **No `useConfigurations()` (`refetchProjectIntegrations`/
 *     `refetchPrivateIntegrations`).** Collapsed into one optional
 *     `onConfigurationCreated` callback fired after a successful inline
 *     configuration create — the caller decides what, if anything, needs
 *     refreshing.
 *  5. **No `useToast`.** `onSaveSuccess`/`onSaveError` callbacks replace
 *     `toastSuccess`/`toastError` (no toast infra in this app yet — see
 *     `OpenAPISchemaInput.tsx`'s fuller citation of this established gap).
 *  6. **`useProjectType()`'s `isTeam` flag** (feeds `useCredentialWarning`)
 *     is a required `isTeamProject` prop instead — see
 *     `useCredentialWarning.hooks.ts`'s own doc comment.
 *  7. `StyledDialog`/`StyledDialogActions`/`StyledDialogContentText`
 *     (`@/components/StyledDialog`, a themed re-export of MUI's `Dialog`
 *     family) -> plain MUI `Dialog`/`DialogActions`/`DialogContentText` —
 *     `shared/ui` has no port of that specific styled wrapper; MUI's own
 *     components provide the same dialog semantics with the default theme.
 *  8. The baseline's `autoFocus` on the "Save"/"Continue editing" buttons
 *     (`ToolkitsOperationButtons.jsx:291,309`) is dropped — `jsx-a11y/no-
 *     autofocus` (R-C1's linter) flags forced focus moves as a usability
 *     hazard for screen-reader/low-vision users, same rationale
 *     `shared/ui`'s own `BaseModal.tsx` doc comment already documents for
 *     an identical Cancel/Confirm pair. `Dialog`'s own default focus
 *     management still gets a keyboard user into the dialog.
 */
export type { SaveToolkitPayload, ToolkitsOperationButtonsProps } from './ToolkitsOperationButtons.types';

function isToolkitNameFlagged(value: unknown): boolean {
  return typeof value === 'object' && value !== null && 'toolkit_name' in value && Boolean((value as { readonly toolkit_name?: unknown }).toolkit_name);
}

function getToolkitName(
  values: Readonly<Record<string, unknown>>,
  toolSchema: ToolkitsOperationButtonsProps['toolSchema'],
): string | undefined {
  const toolkitNameKey = Object.entries(toolSchema?.properties ?? {}).find(([, value]) => isToolkitNameFlagged(value))?.[0];
  if (toolkitNameKey === undefined) return typeof values.name === 'string' ? values.name : undefined;
  const settings = values.settings as Readonly<Record<string, unknown>> | undefined;
  const named = settings?.[toolkitNameKey];
  return typeof named === 'string' ? named : undefined;
}

export function ToolkitsOperationButtons({
  isAdding,
  status,
  setShowValidation = () => undefined,
  onCreateConfiguration,
  onRevertCredentials,
  toolSchema,
  form,
  isTeamProject,
  save,
  projectId,
}: ToolkitsOperationButtonsProps): ReactNode {
  const hasErrors = status?.hasErrors ?? false;
  const hasNotSavedToolConfiguration = status?.hasNotSavedToolConfiguration ?? false;
  const { values: formValues, initialValues: formInitialValues, onReset: onResetForm } = form;
  const { onSave, onSuccess: onSaveSuccess, onError: onSaveError, onConfigurationCreated } = save;
  const validateReasonRef = useRef<string>('');
  const revertCredentialsRef = useRef<(() => void) | undefined>(onRevertCredentials);
  const [openAlert, setOpenAlert] = useState(false);

  useEffect(() => {
    revertCredentialsRef.current = onRevertCredentials;
  }, [onRevertCredentials]);

  const { showWarning, checkBeforeSave, handlers } = useCredentialWarning({
    isCreating: isAdding,
    isTeamProject,
    editToolDetail: asCredentialWarningDetail(formValues),
    originalDetails: asCredentialWarningDetail(formInitialValues),
    revertCredentialsRef,
  });

  const onValidateFailure = useCallback(() => {
    if (validateReasonRef.current) {
      eventEmitter.emit(ToolEvents.ResetValidateEvent, validateReasonRef.current);
      validateReasonRef.current = '';
    }
  }, []);

  const saveToolkit = useCallback(async () => {
    try {
      const toolkitName = getToolkitName(formValues, toolSchema);
      const saved = await onSave({ projectId, toolId: formValues.id as string | number | undefined, values: formValues, name: toolkitName });
      onSaveSuccess?.(saved);
    } catch (caught) {
      onSaveError?.(toolkitFormErrorMessage(caught));
      throw caught;
    }
  }, [formValues, toolSchema, projectId, onSave, onSaveSuccess, onSaveError]);

  const onCloseAlert = useCallback(() => {
    setOpenAlert(false);
    onValidateFailure();
  }, [onValidateFailure]);

  const handleDiscard = useCallback(() => {
    setOpenAlert(false);
    onValidateFailure();
  }, [onValidateFailure]);

  const handleCancel = useCallback(() => {
    setOpenAlert(false);
    onValidateFailure();
    onResetForm?.();
  }, [onValidateFailure, onResetForm]);

  /**
   * Creates a toolkit without configurable properties (`ToolEvents.
   * ToolkitsCreateToolkit`'s listener). `reason` is the event's whole
   * payload (baseline: `async reason => {...}`, `emit(event, data)`'s
   * `data`) — `hasErrors`/`hasNotSavedToolConfiguration` come from this
   * component's own props/closure, not from the emitted payload.
   */
  const handleCreateToolkit = useCallback(
    (reason: string) => {
      validateReasonRef.current = reason;
      if (hasErrors || hasNotSavedToolConfiguration) {
        setShowValidation(true);
        onValidateFailure();
        return;
      }
      eventEmitter.emit(ToolEvents.SaveEvent, validateReasonRef.current);
    },
    [hasErrors, hasNotSavedToolConfiguration, onValidateFailure, setShowValidation],
  );

  /** Creates a toolkit WITH configurable properties — create/select a configuration first, then save. */
  const handleCreateToolkitWithConfiguration = useCallback(
    async (reason: string) => {
      setOpenAlert(false);
      validateReasonRef.current = reason;

      if (hasErrors) {
        setShowValidation(true);
        onValidateFailure();
        return;
      }

      if (hasNotSavedToolConfiguration) {
        const success = await onCreateConfiguration();
        if (success) {
          onConfigurationCreated?.();
        } else {
          onValidateFailure();
        }
        return;
      }

      eventEmitter.emit(ToolEvents.SaveEvent, validateReasonRef.current);
    },
    [hasErrors, hasNotSavedToolConfiguration, onValidateFailure, onCreateConfiguration, setShowValidation, onConfigurationCreated],
  );

  /** Updates an existing toolkit (`ToolEvents.ToolkitsUpdateToolkit`'s listener) — no event payload, mirrors the baseline's own no-arg handler. */
  const handleUpdateToolkit = useCallback(async () => {
    if (hasErrors || hasNotSavedToolConfiguration) {
      setShowValidation(true);
      onValidateFailure();
      return;
    }

    const performSave = async (): Promise<void> => {
      try {
        await saveToolkit();
      } catch {
        // Error already handled in saveToolkit.
      }
    };

    if (checkBeforeSave(() => void performSave())) {
      await performSave();
    }
  }, [hasErrors, hasNotSavedToolConfiguration, saveToolkit, onValidateFailure, setShowValidation, checkBeforeSave]);

  const onValidateEvent = useCallback((reasonFor: string) => {
    validateReasonRef.current = reasonFor;
  }, []);

  useEffect(() => {
    const createToolkitHandler = (payload: unknown): void => handleCreateToolkit(typeof payload === 'string' ? payload : '');
    const createWithConfigurationHandler = (payload: unknown): void => void handleCreateToolkitWithConfiguration(typeof payload === 'string' ? payload : '');
    const updateToolkitHandler = (): void => void handleUpdateToolkit();
    const validateEventHandler = (payload: unknown): void => onValidateEvent(typeof payload === 'string' ? payload : '');

    eventEmitter.on(ToolEvents.ValidateToolEvent, validateEventHandler);
    eventEmitter.on(ToolEvents.ToolkitsCreateToolkit, createToolkitHandler);
    eventEmitter.on(ToolEvents.ToolkitsCreateToolkitWithConfiguration, createWithConfigurationHandler);
    eventEmitter.on(ToolEvents.ToolkitsUpdateToolkit, updateToolkitHandler);

    return () => {
      eventEmitter.off(ToolEvents.ValidateToolEvent, validateEventHandler);
      eventEmitter.off(ToolEvents.ToolkitsCreateToolkit, createToolkitHandler);
      eventEmitter.off(ToolEvents.ToolkitsCreateToolkitWithConfiguration, createWithConfigurationHandler);
      eventEmitter.off(ToolEvents.ToolkitsUpdateToolkit, updateToolkitHandler);
    };
  }, [onValidateEvent, handleCreateToolkit, handleCreateToolkitWithConfiguration, handleUpdateToolkit]);

  return (
    <>
      <Dialog
        open={openAlert}
        onClose={onCloseAlert}
        aria-labelledby="alert-dialog-title"
        aria-describedby="alert-dialog-description"
      >
        <DialogTitle id="alert-dialog-title">
          <Typography
            color="text.secondary"
            variant="headingSmall"
          >
            {t('features.toolkits.toolkitsOperationButtons.missingDataTitle', 'Some fields have missing or invalid data!')}
          </Typography>
        </DialogTitle>
        <DialogContent>
          <DialogContentText id="alert-dialog-description">
            <Typography variant="labelMedium">{t('features.toolkits.toolkitsOperationButtons.chooseAction', 'Choose the action to proceed.')}</Typography>
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          {hasNotSavedToolConfiguration ? (
            <>
              <BaseBtn
                color="alarm"
                variant="elitea"
                onClick={handleDiscard}
              >
                <Typography variant="labelSmall">{isAdding ? t('features.toolkits.toolkitsOperationButtons.deleteToolkit', 'Delete toolkit') : t('features.toolkits.toolkitsOperationButtons.discardChanges', 'Discard changes')}</Typography>
              </BaseBtn>
              <BaseBtn
                color="secondary"
                variant="elitea"
                onClick={handleCancel}
              >
                <Typography variant="labelSmall">{t('features.toolkits.toolkitsOperationButtons.cancel', 'Cancel')}</Typography>
              </BaseBtn>
              <BaseBtn
                color="primary"
                variant="elitea"
                onClick={() => void handleCreateToolkitWithConfiguration(validateReasonRef.current)}
              >
                <Typography variant="labelSmall">{t('features.toolkits.toolkitsOperationButtons.save', 'Save')}</Typography>
              </BaseBtn>
            </>
          ) : (
            <>
              <BaseBtn
                color="alarm"
                variant="elitea"
                onClick={handleDiscard}
              >
                <Typography variant="labelSmall">{isAdding ? t('features.toolkits.toolkitsOperationButtons.deleteToolkit', 'Delete toolkit') : t('features.toolkits.toolkitsOperationButtons.discardChanges', 'Discard changes')}</Typography>
              </BaseBtn>
              <BaseBtn
                color="primary"
                variant="elitea"
                onClick={onCloseAlert}
              >
                <Typography variant="labelSmall">{t('features.toolkits.toolkitsOperationButtons.continueEditing', 'Continue editing')}</Typography>
              </BaseBtn>
            </>
          )}
        </DialogActions>
      </Dialog>
      <CredentialWarningModal
        open={showWarning}
        onConfirm={handlers.onConfirm}
        onCancel={handlers.onCancel}
        onClose={handlers.onClose}
      />
    </>
  );
}
