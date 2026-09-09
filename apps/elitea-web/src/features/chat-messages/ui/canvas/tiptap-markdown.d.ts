/**
 * Ambient typing for `tiptap-markdown` (issue #879) — the package ships no
 * `.d.ts` of its own (verified: `node_modules/tiptap-markdown/dist/` holds
 * only the two bundles + sourcemaps). Typed narrowly to what
 * `./DocumentEditor.tsx` actually calls, the same "declare only the surface
 * this app uses" approach as `src/entries/docs/mdx.d.ts` for `*.mdx`.
 *
 * `editor.storage.markdown` is typed via `@tiptap/core`'s own declared
 * augmentation point (the pattern its other storage-bearing extensions, e.g.
 * `CharacterCount`, document) rather than a cast at every call site.
 *
 * The `export {}` below is load-bearing, not decorative: a `.d.ts` with no
 * top-level `import`/`export` is a GLOBAL script to TypeScript, and every
 * `declare module 'x' { ... }` inside it then REPLACES module `x` wholesale
 * rather than augmenting it — the real `@tiptap/core` typings (`Editor`,
 * `EditorEvents`, …) disappeared project-wide the moment the `Storage`
 * augmentation below was added, until this line made the file a module.
 */
export {};

declare module 'tiptap-markdown' {
  import type { AnyExtension } from '@tiptap/core';

  export interface MarkdownExtensionOptions {
    /** Allow HTML input/output — an unmapped node (e.g. a future custom block) round-trips as raw HTML instead of being dropped. */
    html?: boolean;
    tightLists?: boolean;
    tightListClass?: string;
    bulletListMarker?: string;
    linkify?: boolean;
    breaks?: boolean;
    /** Plain text pasted into the editor is parsed as Markdown (`**bold**` pastes as bold). */
    transformPastedText?: boolean;
    transformCopiedText?: boolean;
  }

  export const Markdown: AnyExtension;
}

declare module '@tiptap/core' {
  interface Storage {
    markdown: {
      getMarkdown: () => string;
    };
  }
}
