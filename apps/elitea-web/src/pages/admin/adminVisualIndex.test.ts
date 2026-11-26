/**
 * The drift gate between the admin ROUTER and the visual-coverage index
 * (issue #229).
 *
 * `parity/screenshot-index.json` carried no `/admin/app/*` row at all, and
 * `scripts/check-visual-coverage.mjs` enforces only the routes that index knows
 * about — so the visual gate could not fail for an admin screen. Not "did not":
 * *could not*, for any change, ever. #225 added persistent navigation to all ten
 * admin pages and altered zero snapshots, with both `Visual regression` and
 * `check-visual-coverage` green.
 *
 * Adding the ten rows fixes today. This test is what stops it happening again:
 * the legal set is read out of `createAdminRouter()`'s OWN table, exactly as
 * `AdminNav.test.tsx` reads it for the nav. A hand-written list here would go
 * stale the first time somebody added an admin page, and the failure mode is
 * silent — a new page simply has no row, so the coverage gate never asks for a
 * spec and reports a green tick for a surface it cannot see.
 *
 * Both directions are asserted, because each catches a different mistake:
 *  - a route with no index row  → a new admin page that nothing would ever gate
 *  - an index row with no route → a row kept alive after its page was deleted,
 *    which the coverage gate would then demand a spec for forever
 */
import { describe, expect, it } from 'vitest';

import screenshotIndex from '../../../parity/screenshot-index.json';
import { createAdminRouter } from './router';

/** Matches `router.tsx`'s `ADMIN_BASE_PATH` and `vite.config.ts`'s admin `base`. */
const ADMIN_BASE_PATH = '/admin/app';

interface IndexShot {
  readonly route: string;
  readonly wiringStatus: string;
}

const shots = screenshotIndex.shots as readonly IndexShot[];

/**
 * The routes the admin router declares, as the index spells them.
 *
 * `__root__` is TanStack Router's synthetic root (it renders `AdminLayout`, not
 * a page). The index `/` is Home under the six areas (ADR-0059 R3.3a, issue
 * 2533), a page of its own, so it is indexed as `/admin/app/`.
 */
function routerAdminRoutes(): string[] {
  return Object.keys(createAdminRouter().routesById)
    .filter((id) => id !== '__root__')
    .map((id) => ADMIN_BASE_PATH + id)
    .sort();
}

function indexedAdminRoutes(): string[] {
  return [
    ...new Set(
      shots.map((shot) => shot.route).filter((route) => route.startsWith(ADMIN_BASE_PATH + '/')),
    ),
  ].sort();
}

describe('the screenshot index covers the admin router', () => {
  it('has a non-trivial route set to check (guards against an empty-router false pass)', () => {
    // Both lists being empty would satisfy the equality below while proving
    // nothing. Ten pages are ported (#200); nine is a floor, not the count, so
    // adding an eleventh does not churn this assertion.
    expect(routerAdminRoutes().length).toBeGreaterThanOrEqual(9);
  });

  it('indexes exactly the routes the admin router declares', () => {
    expect(indexedAdminRoutes()).toEqual(routerAdminRoutes());
  });

  it('marks every admin route wired, so the coverage gate actually enforces them', () => {
    // `check-visual-coverage.mjs` enforces `wiringStatus: wired` and ignores
    // everything else. A row added with any other status would sit in the index
    // looking like coverage while exempting itself from the gate — which is the
    // shape of the defect #229 is about, reintroduced one field at a time.
    const adminShots = shots.filter((shot) => shot.route.startsWith(ADMIN_BASE_PATH + '/'));
    for (const shot of adminShots) {
      expect(shot.wiringStatus, `${shot.route} must be wired`).toBe('wired');
    }
  });
});
