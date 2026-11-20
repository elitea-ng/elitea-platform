/**
 * Starlight route middleware. The What's new page renders its entries through
 * a component (<WhatsNew />), so their headings are not the page's own
 * Markdown headings and Starlight leaves them out of the table of contents.
 * This adds them, in page order, within the configured heading levels.
 */
import { defineRouteMiddleware } from '@astrojs/starlight/route-data';

import { renderedEntries } from './whats-new';

export const WHATS_NEW_PAGE = 'what-is-new';

type TocItem = NonNullable<App.Locals['starlightRoute']['toc']>['items'][number];

function inject(items: TocItem[], item: TocItem): void {
  const last = items.at(-1);
  if (!last || last.depth >= item.depth) items.push(item);
  else inject(last.children, item);
}

export const onRequest = defineRouteMiddleware(async (context) => {
  const route = context.locals.starlightRoute;
  if (route.id !== WHATS_NEW_PAGE || !route.toc) return;
  const { minHeadingLevel, maxHeadingLevel, items } = route.toc;
  for (const { headings } of await renderedEntries()) {
    for (const heading of headings) {
      if (heading.depth >= minHeadingLevel && heading.depth <= maxHeadingLevel) inject(items, { ...heading, children: [] });
    }
  }
});
