import { createStorage } from '@/shared/lib/storage';
import { _loadTokens, isExpired } from './storage';

/** Select only an unexpired server reference for the exact toolkit and resource. */
export function getAuthorizationReference(projectId: string, toolkitId: string, serverUrl: string): string | undefined {
  const saved = createStorage('session').getJSON<SavedReference>(referenceKey(projectId, toolkitId, serverUrl));
  if (saved && Date.parse(saved.expiresAt) > Date.now() && /^[A-Za-z0-9_-]{43}$/.test(saved.reference)) return saved.reference;
  for (const token of Object.values(_loadTokens())) {
    if (token.project_id !== projectId || token.toolkit_id !== toolkitId || isExpired(token)) continue;
    if (token.resource !== serverUrl) continue;
    if (typeof token.authorization_reference === 'string' && /^[A-Za-z0-9_-]{43}$/.test(token.authorization_reference)) return token.authorization_reference;
  }
  return undefined;
}

interface SavedReference { readonly reference: string; readonly expiresAt: string }
function referenceKey(projectId: string, toolkitId: string, serverUrl: string): string {
  return `mcp.testAuthorization.${JSON.stringify([projectId, toolkitId, serverUrl])}`;
}

/** Store only the server capability and its expiry for the exact resource. */
export function saveAuthorizationReference(projectId: string, toolkitId: string | undefined, serverUrl: string | undefined, reference: string | undefined, expiresAt: string | undefined): void {
  if (!toolkitId || !serverUrl || !reference || !expiresAt || !/^[A-Za-z0-9_-]{43}$/.test(reference) || Date.parse(expiresAt) <= Date.now() || !Number.isFinite(Date.parse(expiresAt))) throw new Error('The server did not save a valid authorization reference.');
  createStorage('session').setJSON(referenceKey(projectId, toolkitId, serverUrl), { reference, expiresAt });
}
