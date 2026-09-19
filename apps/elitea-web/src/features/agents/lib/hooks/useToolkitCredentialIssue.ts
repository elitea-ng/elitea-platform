/**
 * #937/ELITEA-1082,1083,1084,1085,1086,1098,1100 — "this attached toolkit's
 * credential does not resolve", for one tool row.
 *
 * `ToolCard.types.ts` has always declared a `validation.banner` slot for
 * exactly this, and no caller anywhere filled it, so the ONE component that
 * renders "Credential setup required:" was reachable from the toolkit's own
 * edit page and nowhere else — not from an Agent's Tools panel, not from a
 * Pipeline's (the same `AgentToolsPanel`/`AgentToolRow` pair serves both),
 * not from a Chat participant card.
 *
 * WHERE EACH HALF LIVES, and why.
 *  - The parsing and the verdict are pure and shared: `entities/credential`'s
 *    `readToolkitCredentialReference`/`isToolkitCredentialMissing`, because
 *    `features/chat-participants` needs the same answer and
 *    `no-sideways-features` forbids it to import this slice.
 *  - The READ is this slice's own (`api/useProjectCredentialTitles.ts`), the
 *    same per-slice duplication `api/configurations.ts` already documents for
 *    the same endpoint family.
 *
 * WHICH PROJECT IS ASKED. A toolkit records `private` alongside the title: a
 * private reference names a credential in the signed-in user's PERSONAL
 * project, a non-private one a credential in the project the toolkit belongs
 * to. Asking the wrong one would report every private credential as missing,
 * so a private reference with no known personal project asks NOTHING and
 * reports NOTHING — absence of a read is not evidence of a missing credential.
 */
import { useRouteContext } from '@tanstack/react-router';

import { isToolkitCredentialMissing, readToolkitCredentialReference, type ToolkitCredentialReference } from '@/entities/credential';

import { useProjectCredentialTitles } from '../../api/useProjectCredentialTitles';

/** Structural, not nominal — same seam and same reason as `api/useSelectedProjectId.ts`'s own context interface. */
interface PersonalProjectIdContext {
  readonly auth?: { readonly getUser?: () => { readonly personal_project_id?: string } | undefined };
}

/** Pure extraction, unit-tested directly (no router needed). */
export function selectPersonalProjectId(context: unknown): string | undefined {
  if (typeof context !== 'object' || context === null) return undefined;
  return (context as PersonalProjectIdContext).auth?.getUser?.()?.personal_project_id;
}

/** The project whose saved credentials a reference must be found in, or `undefined` when it cannot be named. */
export function credentialLookupProjectId(
  reference: ToolkitCredentialReference | null,
  projectId: string | undefined,
  personalProjectId: string | undefined,
): string | undefined {
  if (reference === null) return undefined;
  return reference.isPrivate ? personalProjectId : projectId;
}

export interface ToolkitCredentialIssue {
  readonly reference: ToolkitCredentialReference;
  /** The create-credential route, pre-filled with the type the toolkit needs — same shape `pages/toolkits/lib/credentialPicker.tsx` hands its own banner. */
  readonly createHref: string;
}

export function useToolkitCredentialIssue(settings: unknown, projectId: string | undefined): ToolkitCredentialIssue | null {
  const context: unknown = useRouteContext({ strict: false });
  const reference = readToolkitCredentialReference(settings);
  const lookupProjectId = credentialLookupProjectId(reference, projectId, selectPersonalProjectId(context));
  const titles = useProjectCredentialTitles(lookupProjectId);

  if (reference === null || !isToolkitCredentialMissing(reference, titles)) return null;
  return { reference, createHref: `/credentials/create-credential/${reference.credentialType}` };
}
