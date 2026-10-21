/** Types for `docs-shot-route.mjs` — see that file for why it returns, not throws. */
export declare function resolveShotRoute(
  route: string,
  placeholders: Readonly<Record<string, string>> | undefined,
  seedMap: Record<string, unknown>,
): { route: string; error: undefined } | { route: string; error: string };
