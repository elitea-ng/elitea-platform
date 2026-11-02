/**
 * The signed-in person's personal project id, as a string, from either place
 * the client holds the user: the router context's session user
 * (`auth.getUser()`) or the `/social/author` profile. `undefined` while it is
 * unknown, and for an account whose personal project is not provisioned yet
 * (an empty id).
 *
 * One reader for every shell surface that scopes to the personal project (the
 * bell, the create gate, the desktop's notifications, the chat row menu), so
 * they cannot disagree on a number-vs-string or an empty id.
 */

/** The id off a user record (session user or author profile). */
export function personalProjectIdOf(user: unknown): string | undefined {
  if (typeof user !== 'object' || user === null) return undefined;
  const id = (user as { readonly personal_project_id?: unknown }).personal_project_id;
  if (typeof id === 'number') return Number.isFinite(id) ? String(id) : undefined;
  return typeof id === 'string' && id !== '' ? id : undefined;
}

/** The id off the router context (`auth.getUser()`); pure, so no router is needed to test it. */
export function selectPersonalProjectId(context: unknown): string | undefined {
  if (typeof context !== 'object' || context === null) return undefined;
  const auth = (context as { readonly auth?: { readonly getUser?: () => unknown } }).auth;
  return personalProjectIdOf(auth?.getUser?.());
}
