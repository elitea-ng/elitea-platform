import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { PartitionedBlock, SubAgentGroupable } from '../../lib/subAgentGrouping';
import { SubAgentAccordion } from './SubAgentAccordion';

function block(toolOutputs: unknown): PartitionedBlock {
  const action = { id: 'call-1', type: 'tool', name: 'rp-list-child', toolOutputs } as unknown as SubAgentGroupable;
  return { kind: 'sub', instanceKey: 'call-1', name: 'rp-list-child', actions: [action], pausedForResume: false, aliasKeys: [] };
}

describe('SubAgentAccordion', () => {
  it("previews a saved child's own result, not its response envelope", () => {
    const fenced = '```json\n[\n  {\n    "id": "A"\n  }\n]\n```';
    const { container } = renderWithTheme(<SubAgentAccordion blocks={[block(JSON.stringify({ response: fenced }))]} defaultExpanded />);
    const preview = container.querySelector('pre');
    expect(preview?.textContent).toBe('rp-list-child[\n  {\n    "id": "A"\n  }\n]');
  });

  it('bounds the preview height so a large result scrolls inside the card', () => {
    const { container } = renderWithTheme(<SubAgentAccordion blocks={[block(JSON.stringify({ response: 'row;'.repeat(20000) }))]} defaultExpanded />);
    const style = getComputedStyle(container.querySelector('pre') as Element);
    expect([style.maxHeight, style.overflow]).toEqual(['320px', 'auto']);
  });

  it('keeps any other tool output as it was', () => {
    const { container } = renderWithTheme(<SubAgentAccordion blocks={[block('{"rows":2}')]} defaultExpanded />);
    expect(container.querySelector('pre')?.textContent).toBe('rp-list-child{"rows":2}');
  });
});
