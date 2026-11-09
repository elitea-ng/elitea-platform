import { describe, expect, it } from 'vitest';

import { productLiterals } from './product-literals.mjs';

describe('productLiterals', () => {
  it('finds the product name in strings, templates and JSX text', () => {
    const found = productLiterals(
      "const a = 'Welcome to Elitea';\nconst b = `Open ${x} in ELITEA`;\nconst c = <p>Your EliteA workspace</p>;\nconst d = 'Not reversible from Elitea.';",
      { jsx: true },
    );
    expect(found).toEqual([
      { line: 1, text: 'Welcome to Elitea' },
      { line: 2, text: 'in ELITEA' },
      { line: 3, text: 'Your EliteA workspace' },
      { line: 4, text: 'Not reversible from Elitea.' },
    ]);
  });

  it('ignores comments, identifiers, imports and wire names', () => {
    const source = [
      '// Elitea in a comment',
      "import x from '@/widgets/elitea-section';",
      'const EliteaAssistant = 1;',
      "const h = { 'X-Elitea-Wiki-Chat': 1, env: 'ELITEA_AI_PROJECT_ID', url: 'https://docs.elitea.ai', p: 'elitea/x' };",
      'const ok = `${productName()} MCP Client`;',
    ].join('\n');
    expect(productLiterals(source)).toEqual([]);
  });
});
