import { type ReactNode, useCallback, useMemo } from 'react';

import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { ToolkitWriteBody, ToolkitWriteResult, UseToolkitEditMutation } from '../api/toolkits';

/** A JSON-schema `properties` map narrow enough to find the `toolkit_name`-flagged key — same shape both `SaveToolkitButton`/`CreateToolkitButton` need. */
export interface ToolSchemaLike {
  readonly properties?: Readonly<Record<string, { readonly toolkit_name?: boolean }>>;
}

/** `toolSchema.properties`' `toolkit_name`-flagged key, if any — baseline: `SaveToolkitButton.jsx:35-37`/`CreateToolkitButton.jsx:32-34`, duplicated identically in both. */
export function toolkitNameSettingsKey(toolSchema: ToolSchemaLike | undefined): string | undefined {
  return Object.keys(toolSchema?.properties ?? {}).find((key) => toolSchema?.properties?.[key]?.toolkit_name);
}

export interface ToolkitFormValues extends ToolkitWriteBody {
  readonly id?: string | undefined;
}

/** @public */
export interface SaveToolkitButtonProps {
  readonly toolSchema: ToolSchemaLike | undefined;
  readonly values: ToolkitFormValues;
  readonly isDirty: boolean;
  readonly hasErrors: boolean;
  readonly triggerValidation?: (() => void) | undefined;
  readonly projectId: string | undefined;
  /** Credential-change warning gate (baseline: `entities/credential-warning`'s `checkBeforeSave`) — returns `false` to defer the save (a warning modal will call `performSave` itself once confirmed), `true` to proceed immediately. Omitted entirely skips the check, same as the baseline's `if (onBeforeSave) {...} else { await performSave(); }` branch. */
  readonly onBeforeSave?: ((performSave: () => void) => boolean) | undefined;
  readonly onToolkitSaved?: ((result: ToolkitWriteResult, toolkitData: ToolkitFormValues) => void) | undefined;
  readonly onError?: ((error: unknown) => void) | undefined;
  /** Called when a click is refused because the form still has errors (#2016): the click must say so. */
  readonly onBlocked?: (() => void) | undefined;
  /** No generated `PUT /elitea_core/tool/prompt_lib/{projectId}/{toolId}` endpoint exists yet — see `../api/toolkits.ts`'s module doc comment. Injected so a future caller can wire the real mutation once one exists; this button owns only the click/loading/validation orchestration around it. */
  readonly saveToolkit: UseToolkitEditMutation;
  readonly isSaving?: boolean | undefined;
  readonly sx?: SxProps<Theme>;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/SaveToolkitButton.jsx`.
 *
 * DISCLOSED DEVIATIONS:
 *  - No ambient Formik context (`useFormikContext()`) — `values`/`isDirty`
 *    are explicit props, matching every sibling `features/*` port's
 *    "no Formik" convention (`features/agents/ui/AgentEditor.tsx`'s module
 *    doc comment). `resetForm({ values })` (baseline, on success) is
 *    dropped: this component no longer owns the form instance to reset —
 *    the caller's own form state does that on `onToolkitSaved`.
 *  - `useToolkitEditMutation` is INJECTED (`saveToolkit`), not called
 *    internally — no generated PUT endpoint exists for this route yet (see
 *    `../api/toolkits.ts`'s module doc comment); same "the network call is
 *    the caller's problem once a real endpoint exists" shape
 *    `useValidateToolkit`'s injected `useValidateToolkitQuery` already
 *    established.
 *  - `useToast()` replaced with `onToolkitSaved`/`onError` callbacks — no
 *    toast infrastructure exists in this app (same gap `DeleteToolkitButton`
 *    discloses).
 */
export function SaveToolkitButton({
  toolSchema,
  values,
  isDirty,
  hasErrors,
  triggerValidation,
  projectId,
  onBeforeSave,
  onToolkitSaved,
  onError,
  onBlocked,
  saveToolkit,
  isSaving = false,
  sx,
}: SaveToolkitButtonProps): ReactNode {
  const performSave = useCallback(async () => {
    if (projectId === undefined || values.id === undefined) return;
    try {
      const toolkitNameKey = toolkitNameSettingsKey(toolSchema);
      const toolkitData: ToolkitFormValues = {
        ...values,
        name: toolkitNameKey !== undefined ? ((values.settings?.[toolkitNameKey] as string | undefined) ?? values.name) : values.name,
      };
      const result = await saveToolkit({ projectId, toolId: values.id, ...toolkitData });
      onToolkitSaved?.(result, toolkitData);
    } catch (error) {
      onError?.(error);
    }
  }, [projectId, values, toolSchema, saveToolkit, onToolkitSaved, onError]);

  const onSaveToolkit = useCallback(() => {
    if (hasErrors) {
      triggerValidation?.();
      onBlocked?.();
      return;
    }
    if (onBeforeSave) {
      const shouldProceed = onBeforeSave(() => void performSave());
      if (shouldProceed) void performSave();
      // If shouldProceed is false, `onBeforeSave` is responsible for showing
      // its own warning and eventually re-invoking the `performSave` it was
      // handed — same contract as the baseline (`SaveToolkitButton.jsx:60-67`).
    } else {
      void performSave();
    }
  }, [hasErrors, triggerValidation, onBlocked, onBeforeSave, performSave]);

  const shouldDisableSave = useMemo(() => isSaving || !isDirty, [isSaving, isDirty]);

  return (
    <BaseBtn
      variant="elitea"
      color="primary"
      disabled={shouldDisableSave}
      onClick={onSaveToolkit}
      sx={sx}
    >
      {t('toolkits.saveToolkitButton.label', 'Save')}
      {isSaving && (
        <CircularProgress
          size={20}
          sx={spinnerSx}
        />
      )}
    </BaseBtn>
  );
}

const spinnerSx: SxProps<Theme> = (theme: Theme) => ({ marginLeft: theme.spacing(1) });
