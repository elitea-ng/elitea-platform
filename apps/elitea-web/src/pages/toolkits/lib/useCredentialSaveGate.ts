/**
 * The toolkit editor's save gate for a credential the platform has REFUSED.
 *
 * The legacy screen's rule, restored: a toolkit whose selected credential
 * failed its connection check cannot be saved in ignorance — the Save control
 * is refused and the reason is stated where the user is about to press it.
 *
 * THE RULE IS KEYED ON THE PROBE'S REASON, NOT ON THE STATUS. The stored
 * check answers `auth_failed`/`unreachable` when it reached a verdict about
 * the credential, and answers a plain refusal with NO reason when the
 * deployment could not run the check at all ("Connection checking is not
 * available right now." — `internal/api/v2/configurations/stored_check.go`,
 * which needs a `StoredConfigurationResolver` this build composes only under
 * `ELITEA_CONFIGURATIONS_ENABLED`). Both land on `getCredentialStatus() ===
 * 'invalid'`, so a gate built on the STATUS would refuse every save on every
 * stack that does not compose that resolver — the whole e2e stack included.
 * `useCredentialValidation.getCredentialRefusalReason` already collapses that
 * distinction correctly, and this hook only ever sees the real refusals.
 *
 * The verdict arrives from BELOW: `features/credentials`' check runs inside
 * each credential picker, which is a leaf of the toolkit form, and the Save
 * control lives in the page header above it — so the picker reports up
 * (`./credentialPicker.tsx`'s `onRefusalChange`) and this hook holds what it
 * reported, per field.
 */
import { useCallback, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';

import type { SelectedCredentialRefusal } from './credentialPicker';

export interface CredentialSaveGate {
  /** The sink `useToolkitCredentialPickerSlot` reports each field's verdict to. */
  readonly reportRefusal: (fieldKey: string, refusal: SelectedCredentialRefusal | null) => void;
  /**
   * The sentence to show beside a refused Save, or `undefined` when nothing
   * blocks it. Composed by concatenation, never by interpolation: this app's
   * `t` takes a key and a literal fallback, and a `{{placeholder}}` in the
   * bundle wins over the call site's own fallback, which is how a missing
   * bundle entry turns into literal braces on screen.
   */
  readonly blockedReason: string | undefined;
}

export function useCredentialSaveGate(): CredentialSaveGate {
  const [refusals, setRefusals] = useState<Readonly<Record<string, SelectedCredentialRefusal>>>({});

  const reportRefusal = useCallback((fieldKey: string, refusal: SelectedCredentialRefusal | null): void => {
    setRefusals((prev) => {
      const current = prev[fieldKey];
      if (refusal === null) {
        if (current === undefined) return prev;
        const next = { ...prev };
        delete next[fieldKey];
        return next;
      }
      // Identity churn here would re-render the whole editor on every render
      // of every picker, so an unchanged verdict keeps the previous object.
      if (current !== undefined && current.eliteaTitle === refusal.eliteaTitle && current.refusalReason === refusal.refusalReason && current.message === refusal.message) {
        return prev;
      }
      return { ...prev, [fieldKey]: refusal };
    });
  }, []);

  const blockedReason = useMemo(() => {
    const first = Object.values(refusals)[0];
    if (first === undefined) return undefined;
    const headline = t(
      'pages.toolkits.editToolkit.saveBlockedByCredential',
      'The selected credential did not pass its connection check, so this toolkit cannot be saved.',
    );
    // The SERVER's own words, appended rather than replaced: the headline says
    // what the gate is, the message says what the provider actually answered.
    return first.message === '' ? headline : `${headline} ${first.message}`;
  }, [refusals]);

  return { reportRefusal, blockedReason };
}
