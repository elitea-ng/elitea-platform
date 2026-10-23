/**
 * Guards a path namespace this app SHARES with the backend.
 *
 * `/app/<numeric project id>/<rest>` is this app's project-switch deep link
 * (`src/routes/$projectId.$.tsx` — the `$projectId` param with a `$` splat).
 * It is also, since issue 252, the shape of the MCP SERVER endpoint that
 * `services/elitea-main` serves. The browser edge — elitea-main itself, in
 * compose and in the chart alike (`internal/api/webedge`,
 * `^/app/[0-9]+/mcp(/.*)?$`) — carves it out of this app's `/app` prefix and
 * keeps it; the cluster's Ingress or HTTPRoute is one `/` rule to elitea-main.
 *
 * So the first URL segment `mcp` — exactly `mcp`, followed by `/` or nothing —
 * is RESERVED. Today nothing collides: the routes here are `mcps/...` (plural)
 * and `mcp-auth-callback`, and neither matches. But the overlap is invisible
 * from either side, and the failure it produces does not look like a routing
 * bug: a deep link to a singular `/mcp` route would leave the browser holding
 * elitea-main's `{"error":{"message":"missing authorization header"}}` with a
 * 401, because it sent a session cookie to a JSON API that does not accept
 * cookies for HTML. No E2E journey would catch it either — the E2E stack runs
 * the same compose edge.
 *
 * Hence this test. It reads `parity/route-wiring-map.json`, whose route list is
 * re-derived from `src/routes/**` and gated in CI (`gate-route-wiring-map`), so
 * a new route cannot be added without passing through here.
 *
 * If a singular `/mcp` route is genuinely wanted, the fix is to narrow the
 * edge rule (`webedge.MCPPath`, and the `/app/{project_id}/mcp` path in
 * `services/elitea-main/api/openapi/v2.yaml`'s MCP section) FIRST — not to
 * delete this assertion.
 */
import { describe, expect, it } from 'vitest';

import wiringMap from '../../../parity/route-wiring-map.json';

/** The pathless layout segments TanStack strips from the served URL. */
const PATHLESS = new Set(['_shell']);

const RESERVED_FIRST_SEGMENT = 'mcp';

const routes = wiringMap.routes as readonly { readonly routeFile: string; readonly url: string | null }[];

/** First real URL segment of a route, ignoring pathless layouts and params. */
function firstSegment(url: string): string | undefined {
  return url
    .split('/')
    .filter((segment) => segment !== '' && !PATHLESS.has(segment))
    .at(0);
}

describe('the /app/<projectId>/mcp namespace belongs to the backend MCP server', () => {
  it('no route claims a first segment of exactly "mcp"', () => {
    const offenders = routes
      .filter((route) => typeof route.url === 'string' && firstSegment(route.url) === RESERVED_FIRST_SEGMENT)
      .map((route) => `${route.routeFile} (${route.url})`);

    expect(offenders).toEqual([]);
  });

  // Pins the reason the current MCP routes are safe, so a rename from `mcps`
  // to `mcp` is caught as a deliberate change rather than passing silently
  // through the assertion above with nothing to explain it.
  it('the existing MCP routes sit outside the reserved segment', () => {
    const mcpish = routes
      .filter((route) => typeof route.url === 'string' && (route.url ?? '').includes('mcp'))
      .map((route) => firstSegment(route.url as string));

    expect(mcpish.length).toBeGreaterThan(0);
    expect(new Set(mcpish)).not.toContain(RESERVED_FIRST_SEGMENT);
  });
});
