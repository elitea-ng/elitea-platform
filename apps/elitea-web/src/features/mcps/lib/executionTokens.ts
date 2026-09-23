import { _loadTokens, getTokenInfo, isExpired, needsRefresh } from './storage';
import { triggerProactiveRefresh } from './tokenLifecycle';

type ExecutionToken = { access_token: string; session_id: string | null };

/** Refresh before admission. Keep refresh tokens and client secrets in the OAuth flow. */
export async function getExecutionTokens(projectId: string | undefined): Promise<Record<string, ExecutionToken>> {
  if (!projectId) return {};
  const keys = Object.entries(_loadTokens())
    .filter(([, token]) => token.project_id === projectId)
    .map(([key]) => key);
  // Bound concurrent proxy requests. Concurrent turns share each credential's refresh grant.
  for (const key of keys) {
    if (needsRefresh(key)) await triggerProactiveRefresh(key);
  }
  const result: Record<string, ExecutionToken> = {};
  for (const key of keys) {
    const token = getTokenInfo(key);
    if (!token || token.project_id !== projectId || isExpired(token)) continue;
    if (!token.access_token || token.access_token === '__connection_verified__') continue;
    result[key] = { access_token: token.access_token, session_id: token.session_id ?? null };
  }
  return result;
}
