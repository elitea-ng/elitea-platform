/**
 * Resolves a docs shot's route placeholders from `docs-seed.ts`'s seed map,
 * as a VALUE rather than an exception (#1342).
 *
 * `docs-shots.ts` used to call a throwing resolver before its per-shot
 * `try`, so one shot whose seed key was absent threw out of the capture loop
 * and aborted the whole run. The ci-web-e2e dispatch hit exactly that:
 * `wikiToolkitId` is seeded only with `--wiki-repository` and a real model,
 * the job wires neither, and the two DeepWiki shots took every other shot
 * down with them. Returning the failure keeps it a per-shot result, which
 * `--fail-on-missing` still turns into a red run when the shot was asked for
 * by id.
 *
 * Resolution itself is unchanged: `:name` reads `seedMap[placeholders[name]
 * ?? name]`, and only a string or number resolves — anything else would
 * navigate to a route that still contains a literal `:agentId` and "succeed"
 * as a capture of a 404 page.
 *
 * @param {string} route
 * @param {Readonly<Record<string, string>> | undefined} placeholders
 * @param {Record<string, unknown>} seedMap
 * @returns {{ route: string, error: undefined } | { route: string, error: string }}
 */
export function resolveShotRoute(route, placeholders, seedMap) {
  const unresolved = [];
  const resolved = route.replace(/:([A-Za-z][A-Za-z0-9_]*)/g, (match, name) => {
    const seedMapKey = placeholders?.[name] ?? name;
    const value = seedMap[seedMapKey];
    if (typeof value !== 'string' && typeof value !== 'number') {
      unresolved.push(
        `route placeholder "${match}" (seed map key "${seedMapKey}") did not resolve to a ` +
          `string/number in the seed map — got ${JSON.stringify(value)}`,
      );
      return match;
    }
    return String(value);
  });
  if (unresolved.length > 0) {
    return {
      route,
      error:
        `skipped: ${unresolved.join('; ')}. Run docs-seed.ts with the flags that seed it ` +
        "(e.g. --wiki-repository and a real model for wikiToolkitId), or check the shot's " +
        '`placeholders` override.',
    };
  }
  return { route: resolved, error: undefined };
}
