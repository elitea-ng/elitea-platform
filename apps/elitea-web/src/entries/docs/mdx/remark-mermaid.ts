/**
 * Turns every fenced ```mermaid block into a `<Mermaid chart="...">` element,
 * as a REMARK (mdast) plugin rather than the rehype (hast) transform the
 * PREAMBLE describes ("turns `pre>code.language-mermaid` into a `<Mermaid
 * chart>` component"). The two are equivalent: a fenced code block is mdast's
 * `code` node (`{ type: 'code', lang, value }`) before `remark-rehype` ever
 * lowers it to the `<pre><code class="language-mermaid">` hast shape that
 * description names. Intercepting at the mdast stage means never having to
 * parse a `className` back out of a hast node — this plugin sees `lang`
 * directly.
 *
 * The replacement node type, `mdxJsxFlowElement`, is not a real mdast node —
 * it is `mdast-util-mdx-jsx`'s node for embedded JSX, which is what a literal
 * `<Mermaid chart="..." />` in an `.mdx` file parses to. Emitting it here
 * makes a rendered mermaid fence indistinguishable, at every later stage of
 * the pipeline (remark-rehype passthrough, hast-util-to-estree), from a
 * writer having typed the component by hand. `Mermaid.tsx` is registered
 * in the MDX component map (`components/index.ts`) as any other contract
 * component is.
 */
import type { Code, Root } from 'mdast';
import type { MdxJsxFlowElement } from 'mdast-util-mdx-jsx';
import type { Plugin } from 'unified';
import { visit } from 'unist-util-visit';

export const remarkMermaid: Plugin<[], Root> = () => (tree: Root) => {
  visit(tree, 'code', (node: Code, index, parent) => {
    if (node.lang !== 'mermaid' || parent === undefined || index === undefined) return;
    const chart = node.value;
    const replacement: MdxJsxFlowElement = {
      type: 'mdxJsxFlowElement',
      name: 'Mermaid',
      attributes: [{ type: 'mdxJsxAttribute', name: 'chart', value: chart }],
      children: [],
    };
    parent.children[index] = replacement;
  });
};
