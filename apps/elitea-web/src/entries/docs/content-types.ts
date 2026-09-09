/**
 * The frontmatter shape every `content/*.mdx` page declares (PREAMBLE
 * decision 2: `title`, `description`, optional `updated`). Standalone module
 * rather than declared inline in `mdx.d.ts`'s ambient block, so
 * `content-registry.ts` and `docs-content.test.ts` can both import the type
 * by name instead of relying on TypeScript resolving it out of a wildcard
 * module declaration.
 */
export interface MdxFrontmatter {
  readonly title: string;
  readonly description: string;
  readonly updated?: string;
}
