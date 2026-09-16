import { useRouteContext } from '@tanstack/react-router';

/**
 * "Currently selected project id", for the chat widgets.
 *
 * A local copy of `features/chat-input/api/useSelectedProjectId.ts` (which is
 * itself a copy of `features/agents`', `features/apps`' and
 * `features/toolkits`'), NOT an import of one: `no-sideways-features` forbids
 * a widget reaching into a feature slice, and no shared/entities primitive for
 * "the selected project id" exists yet. See that file's doc comment for the
 * seam rationale in full.
 *
 * The one caller today is `AttachmentButton`, which needs the project whose
 * served file-type allow-list gates what may be attached (#940 A15).
 */

/** Structural, not nominal — the router's registered context type is not imported; this only requires the one method the hook calls. */
interface SelectedProjectIdContext {
  readonly auth?: {
    readonly getSelectedProjectId?: () => string | undefined;
  };
}

function isSelectedProjectIdContext(value: unknown): value is SelectedProjectIdContext {
  return typeof value === 'object' && value !== null;
}

/** Pure extraction, unit-tested directly (no router needed) — the hook below is a one-line wrapper over this. */
export function selectProjectId(context: unknown): string | undefined {
  if (!isSelectedProjectIdContext(context)) return undefined;
  return context.auth?.getSelectedProjectId?.();
}

export function useSelectedProjectId(): string | undefined {
  const context: unknown = useRouteContext({ strict: false });
  return selectProjectId(context);
}
