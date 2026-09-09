/**
 * Screenshot manifest (PREAMBLE decision 4). One entry per `<Screenshot id>`
 * used anywhere under `content/`; `docs-content.test.ts` asserts every
 * `Screenshot` usage has an entry here, and (once the writers/tour capture
 * pass runs `apps/elitea-web/scripts/docs-shots.ts`) that the matching
 * `content/img/<id>.webp` exists and is ≤250 KB.
 *
 * No images are committed yet — this unit only wires the manifest shape and
 * a few entries pointing at real app routes, so `docs-shots.ts` (a later
 * unit) has a real, typed target list to capture against instead of an
 * empty file. `route` is the NEW app's route (this repo's truth source per
 * the PREAMBLE's content rules), not a legacy Mintlify path.
 */

interface ShotAction {
  /** `'click' | 'hover' | 'fill'` kept as a free string: the capture script
   * (docs-shots.ts, a Playwright driver) is the one place that interprets
   * it, and adding an action kind should not require touching this type. */
  readonly type: string;
  readonly selector: string;
  readonly value?: string;
}

export interface Shot {
  readonly id: string;
  /** Root-relative path into the running app, e.g. `/app/agents-hub`. */
  readonly route: string;
  readonly viewport: { readonly width: number; readonly height: number };
  /** Element to screenshot; the full viewport when absent. */
  readonly selector?: string;
  readonly actions?: readonly ShotAction[];
  /** CSS selectors to blank out before capture (timestamps, avatars, …). */
  readonly mask?: readonly string[];
}

const DEFAULT_VIEWPORT = { width: 1440, height: 900 };

export const shots: readonly Shot[] = [
  {
    id: 'chat-home',
    route: '/app/',
    viewport: DEFAULT_VIEWPORT,
  },
  {
    id: 'agents-hub',
    route: '/app/agents-hub',
    viewport: DEFAULT_VIEWPORT,
  },
  {
    id: 'elitea-catalog',
    route: '/app/elitea-catalog',
    viewport: DEFAULT_VIEWPORT,
  },
];
