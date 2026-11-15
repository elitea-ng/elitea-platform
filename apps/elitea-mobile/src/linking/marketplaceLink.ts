import { identity } from '@/config/identity';

/**
 * Marketplace links (ADR-0051: the web's Catalog / Agent HUB is the Marketplace).
 *
 * The web app's addresses, as a person shares or bookmarks them, open the
 * matching screen of the app:
 *
 *   marketplace                       -> /discover        (the Marketplace tab)
 *   marketplace/agents/<id>           -> /discover/<id>   (an agent's sheet)
 *   elitea-catalog, agents-hub        -> the same two, by `?agentId=` (the old shared link)
 *
 * `marketplace/skills/...` and `?kind=skills` open the tab: skills are not on
 * the phone. The link may carry the app's scheme (`elitea://marketplace`) or be
 * the bare path expo-router hands over. A link a person made up (an id that is
 * not a number) opens the tab, never a route that does not exist. The route
 * keeps the name `discover`; only what a person reads says Marketplace.
 */
const AGENT_ID = /^[0-9]{1,15}$/;

const TAB = '/discover';

/** The screen a Marketplace link names, or null when the link is something else. */
export function routeForMarketplaceLink(raw: string): string | null {
  let rest = raw.trim();
  const schemeMatch = /^([a-z][a-z0-9+.-]*):\/\//i.exec(rest);
  if (schemeMatch) {
    const scheme = schemeMatch[1]!.toLowerCase();
    if (!identity.connectSchemes.some((s) => s.toLowerCase() === scheme)) return null;
    rest = rest.slice(schemeMatch[0].length);
  }
  rest = rest.replace(/^\/+/, '');
  const [pathPart, query = ''] = rest.split('?', 2) as [string, string?];
  const segments = pathPart.split('/').filter(Boolean);
  const first = segments[0]?.toLowerCase();
  if (first === 'marketplace') {
    const id = segments[1]?.toLowerCase() === 'agents' ? segments[2] : undefined;
    return id !== undefined && AGENT_ID.test(id) ? `${TAB}/${id}` : TAB;
  }
  if (first === 'elitea-catalog' || first === 'agents-hub') {
    const id = new URLSearchParams(query).get('agentId');
    return id !== null && AGENT_ID.test(id) ? `${TAB}/${id}` : TAB;
  }
  return null;
}
