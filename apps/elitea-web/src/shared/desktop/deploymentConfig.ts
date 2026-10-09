/**
 * The runtime config a browser build gets from the server-written
 * `/app/config.js`, rebuilt for a desktop build that has no such file next to
 * it: every URL is ABSOLUTE against the configured deployment origin (ADR-0029
 * decision 9), and no same-origin assumption survives.
 */

export interface DesktopRuntimeConfig {
  vite_server_url: string;
  vite_base_uri: string;
  vite_public_project_id: string;
}

/** Strip a trailing slash so `origin + '/api/v2'` never doubles one. */
function trimOrigin(origin: string): string {
  return origin.replace(/\/+$/, '');
}

/**
 * Best-effort read of `vite_public_project_id` from the deployment's own
 * `/app/config.js` (a JS assignment, not JSON, and it must never be evaluated).
 * An unreachable or unparsable file yields `''`, which the config schema
 * accepts; the only readers of the id compare it against a project id.
 */
export async function readPublicProjectId(fetchImpl: typeof fetch, origin: string): Promise<string> {
  try {
    const response = await fetchImpl(`${trimOrigin(origin)}/app/config.js`, { credentials: 'omit', redirect: 'error' });
    if (!response.ok) return '';
    const match = /vite_public_project_id\s*:\s*"(\d{0,20})"/.exec(await response.text());
    return match?.[1] ?? '';
  } catch {
    return '';
  }
}

export function desktopRuntimeConfig(origin: string, publicProjectId: string): DesktopRuntimeConfig {
  return {
    vite_server_url: `${trimOrigin(origin)}/api/v2`,
    // The bundled app is served from the root of the app protocol.
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
}
