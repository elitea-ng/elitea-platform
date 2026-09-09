/**
 * Ambient module declaration for `.mdx` imports (vite.config.ts's `docs`
 * mode registers `@mdx-js/rollup`; see the comment there for the full
 * remark/rehype pipeline).
 *
 * The compiled default export is a React component that reads JSX names
 * (`Note`, `Card`, `Mermaid`, …) off its own `components` prop — MDX v3's
 * plain convention when no `providerImportSource` is configured (we do not
 * use `@mdx-js/react`'s context provider; `App.tsx` passes the component map
 * directly on every render instead). `remark-mdx-frontmatter` re-exports the
 * parsed `---` frontmatter block as the named `frontmatter` export.
 */
declare module '*.mdx' {
  import type { ComponentType, ReactNode } from 'react';

  import type { MdxFrontmatter } from './content-types';

  export type { MdxFrontmatter };

  export interface MdxComponentProps {
    readonly components?: Record<string, ComponentType<Record<string, unknown>>>;
    readonly children?: ReactNode;
  }

  const MDXContent: ComponentType<MdxComponentProps>;
  export default MDXContent;
  export const frontmatter: MdxFrontmatter;
}
