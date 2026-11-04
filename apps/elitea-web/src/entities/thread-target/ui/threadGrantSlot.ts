/**
 * The grant field of a thread target, as a slot.
 *
 * The field is the "Run to completion" control of `features/unattended`, and
 * the forms that show a thread target are in other features
 * (`features/pipelines`) and in pages. A feature cannot import another
 * feature (R-L1), so this entity names the field's contract and the app fills
 * it once, in `app/App.tsx` (the root of every build that shows these forms;
 * the admin, maintenance and brand-preview roots do not). Where nothing fills
 * it (a unit test of a form), the form shows no grant field and sends no
 * grant: Main's default.
 */
import { createContext, type ComponentType } from 'react';

import type { ThreadGrantDraft } from '../model/threadTarget';

/** A controlled field: it shows `value` and holds no state of its own. */
export interface ThreadGrantFieldProps {
  /** The draft's grant: the stored grant, Main's default, or the person's edit. */
  readonly value: ThreadGrantDraft;
  /** Called only after the person changes the grant. */
  readonly onChange: (next: ThreadGrantDraft) => void;
  /** Main's 422 `unattended_grant_refused` body, shown under the field. */
  readonly refusal?: unknown;
  readonly disabled?: boolean | undefined;
}

export const ThreadGrantFieldContext = createContext<ComponentType<ThreadGrantFieldProps> | null>(null);
