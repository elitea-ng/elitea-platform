/**
 * Every docs page uses the component contract (Note, Card, Steps, …)
 * without importing it: this prepends the imports to each `.mdx` file.
 * An import a page does not use costs nothing; a component outside this
 * list fails the build, as an undefined component.
 */
import { parse } from 'acorn';
import type { Root } from 'mdast';
import { fileURLToPath } from 'node:url';

const COMPONENTS = new URL('../components/', import.meta.url);

const IMPORTS: Record<string, string> = {
  Accordion: 'Accordion.astro',
  AccordionGroup: 'AccordionGroup.astro',
  Badge: 'Badge.astro',
  Card: 'Card.astro',
  CardGroup: 'CardGroup.astro',
  Check: 'callouts/Check.astro',
  Frame: 'Frame.astro',
  Icon: 'Icon.astro',
  Info: 'callouts/Info.astro',
  Note: 'callouts/Note.astro',
  Screenshot: 'Screenshot.astro',
  Step: 'Step.astro',
  Steps: 'Steps.astro',
  Tab: 'Tab.astro',
  Tabs: 'Tabs.astro',
  Tip: 'callouts/Tip.astro',
  Warning: 'callouts/Warning.astro',
};

export const DOCS_COMPONENTS = Object.keys(IMPORTS);

const source = Object.entries(IMPORTS)
  .map(([name, file]) => `import ${name} from ${JSON.stringify(fileURLToPath(new URL(file, COMPONENTS)))};`)
  .join('\n');

export function remarkDocsComponents() {
  const estree = parse(source, { ecmaVersion: 'latest', sourceType: 'module' });
  return (tree: Root, file: { path?: string }) => {
    if (!file.path?.endsWith('.mdx')) return;
    // mdast has no type for an MDX ESM node; this is the shape remark-mdx parses.
    (tree.children as unknown[]).unshift({ type: 'mdxjsEsm', value: source, data: { estree } });
  };
}
