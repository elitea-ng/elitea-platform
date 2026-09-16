import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { t } from '@/shared/i18n';

import { useSaveIndexConfigurationMutation } from '../../api/indexesApi';
import { useIndexesStore } from '../../model/indexesStore';
import { isIndexConfigDirty, pickIndexConfigValues } from '../helpers/indexConfigDirty';

/**
 * The Indexes tab's "Save" / "Save & Reindex" behaviour (ELITEA-2880 …
 * ELITEA-2887), as one hook.
 *
 * It lives here rather than inside `IndexDetails.tsx` for two reasons, and
 * both are structural rather than stylistic:
 *
 *  - `IndexDetails.tsx` is at its 400-line budget and at its `use-effects`
 *    budget (R-§3.5); `scripts/lib/budgets-core.mjs` attributes an effect to
 *    the innermost UPPERCASE-named enclosing function, so state that lives in
 *    a `use*` hook costs the component nothing. `IndexDetails.helpers.ts`'s own
 *    header records the same reasoning for `useIndexDetailsTabSync`.
 *  - The dirty question and the save are testable on their own, and they are
 *    what the buttons, the notifications and the navigation guard all read.
 *
 * WHAT "SAVED" MEANS HERE. The baseline for the comparison is the index's
 * STORED `metadata.index_configuration`, restricted to the current tool
 * schema's keys — i.e. the values a freshly-opened tab shows, which is what
 * `computeDefaultConfigValues(…, useIndexConfigValues: true)` in
 * `IndexDetails.helpers.ts` puts into the form. After a successful save the
 * hook holds its own baseline, keyed on the index id, so the form goes clean
 * in the same commit as the save rather than waiting for the list query to
 * come back — and a different index selected in the rail drops that override
 * instead of inheriting it.
 */

export interface IndexConfigSaveParams {
  readonly projectId: string | number | undefined;
  readonly toolkitId: string;
  /** The index's `metadata.collection`. Empty for an index that does not exist yet. */
  readonly indexName: string;
  /** Distinguishes one selected index from another — the key the saved-baseline override is held under. */
  readonly indexId: string;
  /** The tool schema's property keys — the editable surface. */
  readonly schemaKeys: readonly string[];
  /** The stored configuration, straight off `index.metadata.index_configuration`. */
  readonly storedConfiguration: Readonly<Record<string, unknown>> | undefined;
  /** The live form values (`toolInputVariables`). */
  readonly current: Readonly<Record<string, unknown>>;
  /** Whether the configuration tab of an EXISTING index is what is on screen. Nothing below applies to the create form or the run tab. */
  readonly isConfigurationTab: boolean;
  /** `validateToolkitForm`'s verdict. A save is refused when it is false — the same gate the reindex already applies. */
  readonly isValidForm: boolean;
  /** Starts the reindex the "Save & Reindex" button ends in — `IndexDetails`' own `handleIndexData`. */
  readonly onReindex: () => void;
  readonly onSuccess?: ((message: string) => void) | undefined;
  readonly onError?: ((message: string) => void) | undefined;
  /**
   * Reports the dirty state upward so the PAGE can arm the app-wide
   * unsaved-changes guard (ELITEA-2885).
   *
   * Upward, and not from here: `widgets/app-shell`'s
   * `useUnsavedChangesNavBlocker` is the one real guard in this app, and a
   * `features/**` file may not import `widgets/**`
   * (`no-upward-from-features`). So the flag is reported and
   * `pages/toolkits/EditToolkit.tsx` arms the guard with it.
   */
  readonly onDirtyChange?: ((dirty: boolean) => void) | undefined;
}

export interface IndexConfigSaveResult {
  /** True when a Save would change something. Drives the buttons AND the unsaved-changes guard. */
  readonly isDirty: boolean;
  /** True while the PUT is in flight — both buttons are disabled for its duration. */
  readonly isSaving: boolean;
  readonly onSave: () => void;
  readonly onSaveAndReindex: () => void;
}

/** The messages the two notifications carry. Exported so the journeys and the unit tests assert the same strings the app renders. */
export const INDEX_CONFIG_SAVE_MESSAGES = {
  saved: t('features.toolkits.indexActions.saveSuccess', 'Configuration saved successfully'),
  savedAndReindexing: t('features.toolkits.indexActions.saveAndReindexSuccess', 'Configuration saved, reindexing started'),
  invalid: t('features.toolkits.indexActions.saveInvalid', 'The configuration is not valid. Fix the highlighted fields and try again.'),
  failed: t('features.toolkits.indexActions.saveFailed', 'The configuration could not be saved'),
} as const;

interface SavedBaselineOverride {
  readonly indexId: string;
  readonly values: Record<string, unknown>;
}

export function useIndexConfigSave(params: IndexConfigSaveParams): IndexConfigSaveResult {
  const {
    projectId,
    toolkitId,
    indexName,
    indexId,
    schemaKeys,
    storedConfiguration,
    current,
    isConfigurationTab,
    isValidForm,
    onReindex,
    onSuccess,
    onError,
    onDirtyChange,
  } = params;

  const saveMutation = useSaveIndexConfigurationMutation();
  const patchIndexMetadata = useIndexesStore((state) => state.updateIndexDepMeta);
  const [override, setOverride] = useState<SavedBaselineOverride | null>(null);
  /**
   * "The save has committed; run the reindex on the NEXT render."
   *
   * Not a direct call, and the difference is the whole of ELITEA-2881. A
   * reindex of an existing index runs `index.metadata.index_configuration`
   * (`useToolkitChat.hooks.ts`'s `resolveRunInputVariables`) — the SERVER's
   * copy, which is what makes "Reindex uses the last SAVED configuration"
   * true. `onReindex` is the closure that read that copy at the last render,
   * so calling it in the same tick as the patch below would dispatch the run
   * with the configuration from BEFORE the save: measured, the run's
   * `tool_params` carried the old value while the PUT carried the new one.
   * Raising a flag defers the call to an effect, which React runs after the
   * render that applied the patch — by which point `onReindex` is the closure
   * that can see it.
   */
  const [reindexAfterSave, setReindexAfterSave] = useState(false);

  const saved = useMemo(() => {
    if (override !== null && override.indexId === indexId) return override.values;
    return pickIndexConfigValues(schemaKeys, storedConfiguration);
  }, [override, indexId, schemaKeys, storedConfiguration]);

  const isDirty = isConfigurationTab && isIndexConfigDirty({ schemaKeys, current, saved });

  /*
   * `current` through a ref for the save path only.
   *
   * The callbacks below are handed to a button that is re-rendered on every
   * keystroke; closing over `current` would make their identity churn for no
   * reason, and — worse — a callback captured before the last keystroke would
   * save the value from before it. The ref is read at CLICK time, which is the
   * only moment the answer is wanted.
   */
  const currentRef = useRef(current);
  currentRef.current = current;

  /** The four identity values as one object — they change together, and grouping them keeps `persist`'s dependency array inside the §3.5 budget. */
  const target = useMemo(() => ({ projectId, toolkitId, indexName, indexId }), [projectId, toolkitId, indexName, indexId]);

  const persist = useCallback(
    async (thenReindex: boolean): Promise<void> => {
      if (target.projectId === undefined || target.indexName === '') return;
      const configuration = pickIndexConfigValues(schemaKeys, currentRef.current);
      try {
        await saveMutation.mutateAsync({
          projectId: target.projectId,
          toolkitId: target.toolkitId,
          indexName: target.indexName,
          configuration,
        });
      } catch {
        // The form keeps its values so the user can correct and retry
        // (ELITEA-2886 step 14): nothing here touches `toolInputVariables`.
        onError?.(INDEX_CONFIG_SAVE_MESSAGES.failed);
        return;
      }
      setOverride({ indexId: target.indexId, values: configuration });
      /*
       * The SERVER's copy, as this browser now knows it.
       *
       * A reindex of an existing index runs `index.metadata.
       * index_configuration` rather than the form
       * (`useToolkitChat.hooks.ts`'s `resolveRunInputVariables`) — that is
       * what makes "Reindex uses the last SAVED configuration" true, and it
       * is deliberately kept. But it means "Save & Reindex" would otherwise
       * store the new configuration and then reindex with the old one: the
       * mutation invalidates the list query, and the refetch lands after the
       * run below has already been dispatched. Patching the row's overlay
       * here closes that window with the value that was just committed.
       */
      patchIndexMetadata(target.indexId, { index_configuration: configuration });
      onSuccess?.(thenReindex ? INDEX_CONFIG_SAVE_MESSAGES.savedAndReindexing : INDEX_CONFIG_SAVE_MESSAGES.saved);
      // Strictly AFTER the save has committed. "Save & Reindex validates and
      // saves config FIRST, then starts reindexing" (ELITEA-2881) — a reindex
      // fired in parallel would race the write it is supposed to consume. The
      // flag, rather than the call, is what makes it read the saved value —
      // see `reindexAfterSave`.
      if (thenReindex) setReindexAfterSave(true);
    },
    [target, schemaKeys, saveMutation, patchIndexMetadata, onSuccess, onError],
  );

  const run = useCallback(
    (thenReindex: boolean) => {
      // The validation gate is the same one the Index/Reindex buttons use, and
      // it refuses BOTH halves: an invalid configuration must not be stored and
      // must not start a run (ELITEA-2882).
      if (!isValidForm) {
        onError?.(INDEX_CONFIG_SAVE_MESSAGES.invalid);
        return;
      }
      void persist(thenReindex);
    },
    [isValidForm, onError, persist],
  );

  const onSave = useCallback(() => run(false), [run]);
  const onSaveAndReindex = useCallback(() => run(true), [run]);

  useEffect(() => {
    if (!reindexAfterSave) return;
    setReindexAfterSave(false);
    onReindex();
  }, [reindexAfterSave, onReindex]);

  /*
   * Unmount always lowers the flag. The guard's own store is a module
   * singleton, so an index panel that is closed (a different tab, a different
   * toolkit, a route change the user already confirmed) must not leave the
   * whole app blocked on edits that are no longer on screen.
   */
  useEffect(() => {
    onDirtyChange?.(isDirty);
    return () => onDirtyChange?.(false);
  }, [isDirty, onDirtyChange]);

  return { isDirty, isSaving: saveMutation.isPending, onSave, onSaveAndReindex };
}
