/**
 * Starlight builds a page's table of contents from the headings in its own
 * Markdown. The Upgrading page's entries are rendered by a component
 * (src/components/UpgradeNotes.astro), so their headings are not there:
 * this adds one table-of-contents item per entry, in page order.
 */
import { defineRouteMiddleware } from '@astrojs/starlight/route-data';

import { UPGRADING_PAGE, plainTitle, upgradeNotes } from './upgrading';

export const onRequest = defineRouteMiddleware(async (context) => {
  const route = context.locals.starlightRoute;
  if (route.id !== UPGRADING_PAGE || route.toc === undefined) return;
  const notes = await upgradeNotes();
  route.toc.items.push(...notes.map((note) => ({ depth: 2, slug: note.id, text: plainTitle(note.data.title), children: [] })));
});
