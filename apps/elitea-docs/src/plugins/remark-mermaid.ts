/**
 * A ```mermaid fence becomes a diagram placeholder holding the chart source;
 * src/components/MermaidLoader.astro renders every placeholder on the page
 * in the browser, with the Elitea theme of the current colour scheme. The
 * source stays readable (a <pre>) until then, and if rendering fails.
 */
import type { Code, Html, Root } from 'mdast';
import { visit } from 'unist-util-visit';

function escapeHtml(value: string): string {
  return value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
}

export function remarkMermaid() {
  return (tree: Root) => {
    visit(tree, 'code', (node: Code, index, parent) => {
      if (node.lang !== 'mermaid' || parent === undefined || index === undefined) return;
      const html: Html = {
        type: 'html',
        value: `<figure class="docs-mermaid not-content"><pre class="docs-mermaid__source">${escapeHtml(node.value)}</pre></figure>`,
      };
      parent.children[index] = html;
    });
  };
}
