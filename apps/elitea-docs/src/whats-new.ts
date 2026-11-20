/**
 * The What's new entries (src/content/whats-new/<YYYY-MM-DD>-<slug>.md),
 * newest first: by `date`, then by `order` (higher first, default 0), then
 * by file name. Each entry's body starts with its own `## ` heading, so the
 * anchors come from the same Markdown pipeline as every other page.
 */
import type { MarkdownHeading } from 'astro';
import { type CollectionEntry, getCollection, render } from 'astro:content';

type Entry = CollectionEntry<'whatsNew'>;

export function compareEntries(a: Entry, b: Entry): number {
  // Code-unit order for the file name, not localeCompare: the same page on
  // every machine, whatever its ICU data.
  const byName = a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  return b.data.date.getTime() - a.data.date.getTime() || b.data.order - a.data.order || byName;
}

let rendered: Promise<{ Content: Awaited<ReturnType<typeof render>>['Content']; headings: MarkdownHeading[] }[]> | undefined;

/**
 * Every entry rendered, newest first. A build renders them once for the page
 * and its table of contents; `astro dev` renders them per request, so an
 * added or edited entry shows without a restart.
 */
export function renderedEntries() {
  if (import.meta.env.DEV) return renderAll();
  rendered ??= renderAll();
  return rendered;
}

function renderAll() {
  return getCollection('whatsNew').then((entries) =>
    Promise.all(
      entries.sort(compareEntries).map(async (entry) => {
        const { Content, headings } = await render(entry);
        return { Content, headings };
      }),
    ),
  );
}
