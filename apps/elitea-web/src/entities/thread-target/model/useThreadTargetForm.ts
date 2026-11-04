/**
 * The state of the "Runs" choice in a schedule or a trigger form: the choice,
 * the thread draft, and the errors on its fields. The form reads `pending()`
 * for the target fields of its save, and `submit()` puts Main's refusal of
 * that save on the field it is about.
 *
 * The state is seeded from the stored row when the form opens and when the
 * stored target itself changes (a save answered with it). A refetch that
 * answers the same target does not seed it again: it must not discard what
 * the person typed.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import type { TargetChoice, ThreadTargetSectionProps, VersionKind } from '../ui/ThreadTargetSection';
import {
  isThreadTarget,
  threadTargetBody,
  threadTargetDraftOf,
  threadTargetRefusal,
  type StoredThreadTarget,
  type ThreadTargetDraft,
  type ThreadTargetErrors,
  type ThreadTargetFieldsBody,
} from './threadTarget';

/** The target fields a save sends: none (the stored target stays), the fields, or none because a field is not complete. */
type PendingTarget = { readonly kind: 'unchanged' } | { readonly kind: 'body'; readonly body: ThreadTargetFieldsBody } | { readonly kind: 'invalid' };

export interface ThreadTargetForm {
  /** The props of `ThreadTargetSection`, less `projectId`. */
  readonly section: Omit<ThreadTargetSectionProps, 'projectId'>;
  /** True when the choice or the thread fields differ from the stored row. */
  readonly changed: boolean;
  /** The target fields of the next save; marks the fields that are not complete. */
  readonly pending: () => PendingTarget;
  /**
   * Runs a save that resolves to the error it caught (or nothing): true when
   * Main did not refuse the target, false when it did (the refusal now shows
   * on its field).
   */
  readonly submit: (save: () => unknown) => Promise<boolean>;
}

/** A key for the stored target: the form seeds again when it changes. */
function storedKey(stored: StoredThreadTarget | undefined): string {
  return isThreadTarget(stored) ? JSON.stringify([stored?.conversation_uuid, stored?.prompt, stored?.grant]) : '';
}

/** What a draft would save: the grant field's own `edit` state is not part of it. */
function draftKey(draft: ThreadTargetDraft): string {
  const grant = draft.grant.kind === 'set' ? draft.grant.grant : draft.grant.kind;
  return JSON.stringify([draft.conversationUuid, draft.prompt, grant]);
}

export function useThreadTargetForm(open: boolean, stored: StoredThreadTarget | undefined, versionKind: VersionKind): ThreadTargetForm {
  const key = storedKey(stored);
  const storedIsThread = key !== '';
  const [choice, setChoice] = useState<TargetChoice>(storedIsThread ? 'thread' : 'version');
  const [draft, setDraft] = useState<ThreadTargetDraft>(() => threadTargetDraftOf(stored));
  const [errors, setErrors] = useState<ThreadTargetErrors | undefined>(undefined);

  useEffect(() => {
    if (!open) return;
    setChoice(storedIsThread ? 'thread' : 'version');
    setDraft(threadTargetDraftOf(stored));
    setErrors(undefined);
    // Seeded on open and on a new stored target only (`key`); see the module comment.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, key]);

  const changed = choice !== (storedIsThread ? 'thread' : 'version') || (choice === 'thread' && draftKey(draft) !== draftKey(threadTargetDraftOf(stored)));

  const pending = useCallback((): PendingTarget => {
    if (choice === 'version') return storedIsThread ? { kind: 'body', body: { target_kind: versionKind } } : { kind: 'unchanged' };
    const result = threadTargetBody(draft);
    if (result.ok) return { kind: 'body', body: result.body };
    setErrors(result.errors);
    return { kind: 'invalid' };
  }, [choice, draft, storedIsThread, versionKind]);

  const submit = useCallback(async (save: () => unknown): Promise<boolean> => {
    setErrors(undefined);
    const refusal = threadTargetRefusal(await save());
    if (refusal === undefined) return true;
    setErrors(refusal);
    return false;
  }, []);

  const section = useMemo(
    () => ({
      versionKind,
      choice,
      onChoiceChange: (next: TargetChoice) => {
        setChoice(next);
        setErrors(undefined);
      },
      draft,
      onDraftChange: (next: ThreadTargetDraft) => {
        setDraft(next);
        setErrors(undefined);
      },
      errors,
    }),
    [versionKind, choice, draft, errors],
  );

  return { section, changed, pending, submit };
}
