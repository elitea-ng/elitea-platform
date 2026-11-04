/**
 * The grant field of a thread target, as a slot.
 *
 * The field is the "Run to completion" control of `features/unattended`, and
 * the forms that show a thread target are in other features
 * (`features/pipelines`) and in pages. A feature cannot import another
 * feature (R-L1), so this entity names the field's contract and the app fills
 * it once (`app/providers`). Where nothing fills it (a unit test of a form),
 * the form shows no grant field and sends no grant: Main's default.
 */
import { createContext, type ComponentType } from 'react';

import type { ChatUnattendedRequest } from '@/shared/api/generated/model';

import type { ThreadGrantDraft } from '../model/threadTarget';

export interface ThreadGrantFieldProps {
  /** The stored grant that seeds the field; absent for a new target. */
  readonly stored: ChatUnattendedRequest | undefined;
  /** Called only after the person changes the grant. */
  readonly onChange: (next: ThreadGrantDraft) => void;
  /** Main's 422 `unattended_grant_refused` body, shown under the field. */
  readonly refusal?: unknown;
  readonly disabled?: boolean | undefined;
}

export const ThreadGrantFieldContext = createContext<ComponentType<ThreadGrantFieldProps> | null>(null);
