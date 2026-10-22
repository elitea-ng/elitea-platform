import { getConfig } from '@/shared/config';

/**
 * Router basename (spec §7.1 contract rows C3/C6; §9.3 unit R2 task 5).
 *
 * Old-app parity (`apps/elitea-ui/src/routes.js:129-131` in the old EliteaUI
 * app; the submodule that held it has been removed from this repository):
 *
 *   export const getBasename = () => (DEV ? '' : VITE_BASE_URI);
 *
 * Two deliberate deviations from that literal shape, both forced by the new
 * config contract rather than chosen freely:
 *
 *  - The old `DEV` was itself `getEnvVar('DEV')` — a RUNTIME-CONFIG key.
 *    Spec §7.1 C7b / §5.4 removes exactly that key (`dev`) from the config
 *    schema, because it is what let the nginx entrypoint leak a dev bearer
 *    token into a world-readable `config.js` in production. `import.meta.env.DEV`
 *    is the modern equivalent: it is Vite's OWN build-time flag, true only
 *    under `vite dev`, statically `false` — and therefore dead-code-eliminated —
 *    in every production bundle. Unit F4's `http.ts` already gates its dev-token
 *    header the same way (`import.meta.env.DEV`), so this keeps the app's one
 *    "are we in dev" signal consistent across units.
 *  - A missing config can never actually reach this function in production
 *    use: `App.tsx` renders `MissingEnvPage` and returns before
 *    `AppProviders` — and therefore before any router that would call this —
 *    ever mounts. This function still returns a safe `''` fallback rather
 *    than throwing on that branch, in keeping with §3.6 ("errors are values
 *    at the boundary"): a basename resolver has no boundary to report a
 *    value AT, so degrading to the DEV-mode behaviour (root-relative) is the
 *    least surprising thing it can do if it is ever called out of that
 *    normal sequence (e.g. from a test that does not go through `App.tsx`).
 *
 * @public Wave-1 surface — no consumer yet inside this unit; this is the
 * dedicated export unit R1 reads to build `createRouter({ basename })` once
 * its router shell lands (spec §9.3 unit R1, C3's "Verified by: unit R1").
 */
export function getAppBasename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_base_uri : '';
}
