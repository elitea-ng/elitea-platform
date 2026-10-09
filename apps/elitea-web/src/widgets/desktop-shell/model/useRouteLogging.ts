/**
 * Route transitions to the host log, at debug (README, "Logs"): the path
 * (never the search, which can carry ids worth nothing to support and codes
 * worth something to an attacker), the router's status, and each matched
 * route with its own status — what tells "pending forever" apart from a
 * guard that never redirected.
 */
import { useRouterState } from '@tanstack/react-router';
import { useEffect } from 'react';

import { hostLog } from '@/shared/desktop/diagnostics';

export function describeRouterState(state: {
  status: string;
  location: { pathname: string };
  matches: readonly { routeId: string; status: string }[];
}): string {
  const matches = state.matches.map((match) => `${match.routeId}:${match.status}`).join(' > ');
  return `${state.location.pathname} [${state.status}] ${matches}`;
}

export function useRouteLogging(): void {
  const line = useRouterState({ select: describeRouterState });
  useEffect(() => {
    hostLog('debug', line, 'route');
  }, [line]);
}
