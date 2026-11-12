/**
 * Opens the create-credential page in a NEW tab from inside the toolkit form.
 *
 * The form's unsaved edits live in this tab's memory. Navigating this tab to
 * the create page dropped them with no warning (issue 1965), so the page opens
 * beside it instead; the picker's refresh action picks the new credential up
 * when the person comes back. `noopener` keeps the new tab from reaching this
 * one through `window.opener`.
 */
import type { AnyRouter } from '@tanstack/react-router';

/** Route `/_shell/credentials/create-credential/$credentialType` (`src/routes/_shell/credentials/create-credential.$credentialType.tsx`). */
const CREATE_CREDENTIAL_ROUTE = '/credentials/create-credential/$credentialType';

export function openCreateCredentialTab(router: Pick<AnyRouter, 'buildLocation'>, credentialType: string): void {
  const { href } = router.buildLocation({ to: CREATE_CREDENTIAL_ROUTE, params: { credentialType } });
  window.open(href, '_blank', 'noopener');
}
