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
  return b.data.date.getTime() - a.data.date.getTime() || b.data.order - a.data.order || a.id.localeCompare(b.id);
}

let rendered: Promise<{ Content: Awaited<ReturnType<typeof render>>['Content']; headings: MarkdownHeading[] }[]> | undefined;

/** Every entry rendered, newest first; computed once per build. */
export function renderedEntries() {
  rendered ??= getCollection('whatsNew').then((entries) =>
    Promise.all(
      entries.sort(compareEntries).map(async (entry) => {
        const { Content, headings } = await render(entry);
        return { Content, headings };
      }),
    ),
  );
  return rendered;
}
