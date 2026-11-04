/**
 * Public API — spec §3.3: named exports only.
 *
 * The `workspace_thread` target of a schedule or a webhook trigger (ADR-0045
 * decision 9, client contract 1.34): the "Runs" choice with its thread picker,
 * message and grant slot, the save fields and the refusal by field, and the
 * "Runs on <thread>" line of a card.
 */
export { isThreadTarget, threadTargetRefusal, type StoredThreadTarget, type ThreadGrantDraft, type ThreadTargetFieldsBody } from './model/threadTarget';
export { useThreadTargetForm, type ThreadTargetForm } from './model/useThreadTargetForm';
export { ThreadTargetSection } from './ui/ThreadTargetSection';
export { ThreadTargetLine } from './ui/ThreadTargetLine';
export { ThreadGrantFieldContext, type ThreadGrantFieldProps } from './ui/threadGrantSlot';
