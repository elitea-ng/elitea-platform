import type { Credential } from './types';

/**
 * apps/elitea-ui/src/[fsd]/features/credentials/lib/helpers/
 * credentialName.helpers.js:3-10 `extraCredentialName`, ported verbatim:
 * removes the FIRST occurrence of the literal `integration_` (not
 * anchored to the start — `String.replace` with a string argument), then
 * strips everything up to and including the LAST `Provider_` (`.*` is
 * greedy), replaces every `_` with a space, capitalizes the first letter,
 * and trims.
 */
export function providerDisplayName(type: string): string {
  const withoutIntegration = type.replace('integration_', '');
  const withoutProvider = withoutIntegration.replace(/.*Provider_/, '');
  const spaced = withoutProvider.replace(/_/g, ' ');
  const capitalized = spaced.charAt(0).toUpperCase() + spaced.slice(1);
  return capitalized.trim();
}

/**
 * apps/elitea-ui/src/[fsd]/features/credentials/lib/helpers/
 * credential.helpers.js:32-73 `enhanceCredentialData` — display-name
 * fallback chain: `label` -> `elitea_title` -> `data.title` -> the
 * provider display name derived from `type`.
 */
export function credentialDisplayName(credential: Credential): string {
  // `typeof`, not `!== undefined`: the wire sends `label: null` for an
  // unlabelled credential, and `null.trim()` took the whole list down.
  const { label, eliteaTitle } = credential as { readonly label?: unknown; readonly eliteaTitle?: unknown };
  if (typeof label === 'string' && label.trim() !== '') return label;
  if (typeof eliteaTitle === 'string' && eliteaTitle.trim() !== '') return eliteaTitle;
  const dataTitle = credential.data?.['title'];
  if (typeof dataTitle === 'string' && dataTitle.trim() !== '') return dataTitle;
  return providerDisplayName(credential.type);
}

/** `credential.helpers.js:32-73` — `credential_url = data?.base_url || data?.url || ''`. */
export function credentialUrl(credential: Credential): string {
  const baseUrl = credential.data?.['base_url'];
  if (typeof baseUrl === 'string' && baseUrl !== '') return baseUrl;
  const url = credential.data?.['url'];
  return typeof url === 'string' ? url : '';
}

/**
 * apps/elitea-ui/src/[fsd]/features/credentials/lib/helpers/
 * credential.helpers.js:32-73 — `project_scope = credential.project_id ?
 * 'Local' : 'Inherited'`.
 */
export function credentialScope(credential: Credential): 'Local' | 'Inherited' {
  return credential.projectId !== undefined && credential.projectId !== '' ? 'Local' : 'Inherited';
}

/**
 * apps/elitea-ui/src/common/utils.jsx:613-617 `pinnedComparator` — pinned
 * items sort first, ported for credentials
 * (apps/elitea-ui/src/hooks/credentials/useLoadCredentials.js:141).
 */
export function sortCredentialsPinnedFirst(credentials: readonly Credential[]): Credential[] {
  return [...credentials].sort((a, b) => {
    if (a.isPinned === b.isPinned) return 0;
    return a.isPinned === true ? -1 : 1;
  });
}
