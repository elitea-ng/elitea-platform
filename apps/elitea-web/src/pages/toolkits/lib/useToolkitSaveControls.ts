/**
 * Everything the toolkit edit page's Save/Cancel control needs, in one hook
 * so `../EditToolkit.tsx` stays inside the §3.5 400-line budget.
 *
 * ── HOW THE SAVE ACTUALLY REACHES THE NETWORK ──────────────────────────────
 * `features/toolkits`' `ToolkitsOperationButtons` already owns the update
 * orchestration — the validation guard, the credential-change warning modal,
 * and the call to `ToolkitForm`'s `onSave` (which `ConfigurationTab` turns
 * into the real `PUT`, recording a per-field server refusal on the way, #613).
 * It has always listened for `ToolEvents.ToolkitsUpdateToolkit`, and until now
 * NOTHING in this app emitted it, so that whole path was unreachable and the
 * page had no Save at all. This hook is the emitter the baseline puts in its
 * own toolkit tab bar. The seam is wired, not duplicated: there is exactly one
 * save path on this screen.
 *
 * `eventEmitter` moved to `shared/lib` to make that legal — the listener is
 * inside a slice and the emitter is a page, and `no-deep-slice-import` (R-L3)
 * lets `pages/` enter a slice only through its full `index.ts`. See that
 * module's own doc comment.
 *
 * ── WHY `isSaving` CANNOT GET STUCK ────────────────────────────────────────
 * An emit cannot be awaited, so the flag is cleared by the save's own
 * outcome callbacks. Both are reachable for every save this control can
 * start: `onSave` returns BEFORE raising the flag when the form has errors,
 * this page never sets `hasNotSavedCredentials` (so the listener's other
 * guard cannot short-circuit), and the credential-change warning needs
 * `isTeamProject`, which `ToolkitForm` defaults to `false`. Every emit
 * therefore ends in `onSaveSuccess` or `onSaveError`.
 */
import { useCallback, useState } from 'react';

import { ToolEvents } from '@/entities/toolkit';
import { eventEmitter } from '@/shared/lib/eventEmitter';

import { useCredentialSaveGate } from './useCredentialSaveGate';
import type { CredentialSaveGate } from './useCredentialSaveGate';

/** `ToolkitForm`'s own validation report, republished by `ConfigurationTab`. Not exported: `ToolkitSaveControls` names it structurally and nothing outside this file needs the name. */
interface ToolkitValidationState {
  readonly hasErrors: boolean;
  readonly triggerValidation: () => void;
}

export interface UseToolkitSaveControlsParams {
  /** The page's own edit-dirty flag; the Save/Cancel pair is gated on it exactly as the agent editor's is. */
  readonly isDirty: boolean;
  /** Runs after a save the server accepted — the page clears its dirty flag there. */
  readonly onSaved: () => void;
  /** Runs when the user confirms Cancel — the page restores the fetched detail there. */
  readonly onDiscarded: () => void;
}

export interface ToolkitSaveControls {
  readonly canSave: boolean;
  readonly isSaving: boolean;
  readonly disabledReason: string | undefined;
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  /** Wire to `ConfigurationTab`'s `onValidationStateChange`. */
  readonly onValidationStateChange: (state: ToolkitValidationState) => void;
  /** Wire to `ConfigurationTab`'s `saveHandlers.onSaveSuccess`. */
  readonly onSaveSuccess: () => void;
  /** Wire to `ConfigurationTab`'s `saveHandlers.onSaveError`. */
  readonly onSaveError: (message: string) => void;
  /** Wire to `useToolkitCredentialPickerSlot`'s refusal sink. */
  readonly reportCredentialRefusal: CredentialSaveGate['reportRefusal'];
  /** The last save's failure message, or `undefined`. Rendered as the page's own alert. */
  readonly saveError: string | undefined;
}

function noopTriggerValidation(): void {}

const CLEAN_VALIDATION: ToolkitValidationState = { hasErrors: false, triggerValidation: noopTriggerValidation };

export function useToolkitSaveControls({ isDirty, onSaved, onDiscarded }: UseToolkitSaveControlsParams): ToolkitSaveControls {
  const [validation, setValidation] = useState<ToolkitValidationState>(CLEAN_VALIDATION);
  const [isSaving, setIsSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const gate = useCredentialSaveGate();

  const onSave = useCallback(() => {
    // The same order `SaveToolkitButton` uses: an invalid form REVEALS its
    // errors rather than silently doing nothing.
    if (validation.hasErrors) {
      validation.triggerValidation();
      return;
    }
    setSaveError(undefined);
    setIsSaving(true);
    eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
  }, [validation]);

  const onSaveSuccess = useCallback(() => {
    setIsSaving(false);
    onSaved();
  }, [onSaved]);

  const onSaveError = useCallback((message: string) => {
    setIsSaving(false);
    setSaveError(message === '' ? undefined : message);
  }, []);

  const onDiscard = useCallback(() => {
    setSaveError(undefined);
    onDiscarded();
  }, [onDiscarded]);

  // A plain object, not a `useMemo`: memoising it would need nine
  // dependencies (over the §3.5 hook-deps budget of 8) to save nothing — the
  // page destructures every field, and each field is already stable on its
  // own.
  return {
    // Dirty ONLY — `SaveToolkitButton`'s own gate. An invalid form keeps a
    // clickable Save so the click can REVEAL the errors (`onSave` above);
    // disabling it there leaves the user with a refused button and no
    // statement of what is wrong.
    canSave: isDirty,
    isSaving,
    disabledReason: gate.blockedReason,
    onSave,
    onDiscard,
    onValidationStateChange: setValidation,
    onSaveSuccess,
    onSaveError,
    reportCredentialRefusal: gate.reportRefusal,
    saveError,
  };
}
