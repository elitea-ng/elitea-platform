import { useState } from 'react';

import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { dumpYaml } from '../../lib/dumpYaml.helpers';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument, YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { CommonInterruptSettings } from './CommonInterruptSettings';
import { LlmOutputContractSettings } from './LlmOutputContractSettings';

const DOCUMENT: YamlPipelineDocument = {
  entry_point: 'LLM_1',
  nodes: [{ id: 'LLM_1', type: 'llm', transition: 'END', structured_output: false }],
};

/** A live document, so each write is read back the way the editor reads it. */
function renderNode(initial: YamlPipelineDocument) {
  const latest: { document: YamlPipelineDocument } = { document: initial };
  function Harness() {
    const [document, setDocument] = useState(initial);
    latest.document = document;
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: document, setYamlJsonObject: setDocument });
    return (
      <FlowEditorContext.Provider value={contextValue}>
        <LlmOutputContractSettings id="LLM_1" />
        <CommonInterruptSettings id="LLM_1" type="llm" />
      </FlowEditorContext.Provider>
    );
  }
  renderWithTheme(<Harness />);
  return latest;
}

function node(document: YamlPipelineDocument): YamlPipelineNode {
  const found = document.nodes?.[0];
  if (found === undefined) throw new Error('no node');
  return found;
}

/** The node as the saved YAML holds it: dumped to text and loaded back. */
function savedNode(document: YamlPipelineDocument): Record<string, unknown> {
  const parsed = load(dumpYaml(document)) as { nodes: Record<string, unknown>[] };
  return parsed.nodes[0] ?? {};
}

describe('LlmOutputContractSettings', () => {
  it('writes the contract into the node YAML as a mapping', async () => {
    const user = userEvent.setup();
    const latest = renderNode(DOCUMENT);

    await user.click(screen.getAllByRole('combobox')[0] as HTMLElement);
    await user.click(screen.getByRole('option', { name: 'Regular expression' }));
    fireEvent.change(screen.getByTestId('llm-node-output-contract-pattern'), { target: { value: '^yes|no$' } });

    expect(node(latest.document)['output_contract']).toEqual({ type: 'regex', pattern: '^yes|no$' });
    const yaml = dumpYaml(latest.document);
    expect(yaml).toContain('    output_contract:\n      type: regex\n      pattern: ^yes|no$\n');
    expect(savedNode(latest.document)['output_contract']).toEqual({ type: 'regex', pattern: '^yes|no$' });
  });

  it('reads a stored contract and removes the key on None', async () => {
    const user = userEvent.setup();
    const stored: YamlPipelineDocument = {
      ...DOCUMENT,
      nodes: [{ ...node(DOCUMENT), output_contract: { type: 'json_schema', schema: { type: 'object' } } }],
    };
    const latest = renderNode(stored);
    expect(screen.getByTestId('llm-node-output-contract-schema')).toHaveValue('{\n  "type": "object"\n}');

    await user.click(screen.getAllByRole('combobox')[0] as HTMLElement);
    await user.click(screen.getByRole('option', { name: 'None' }));

    expect(Object.hasOwn(node(latest.document), 'output_contract')).toBe(false);
    expect(dumpYaml(latest.document)).not.toContain('output_contract');
  });

  it('hides the field and removes the contract when structured output is on', async () => {
    const user = userEvent.setup();
    const stored: YamlPipelineDocument = {
      ...DOCUMENT,
      nodes: [{ ...node(DOCUMENT), output_contract: { type: 'format', format: 'json' } }],
    };
    const latest = renderNode(stored);
    expect(screen.getByTestId('llm-node-output-contract')).toBeInTheDocument();

    await user.click(screen.getByRole('switch', { name: 'Structured output' }));

    expect(node(latest.document)['structured_output']).toBe(true);
    expect(Object.hasOwn(node(latest.document), 'output_contract')).toBe(false);
    expect(screen.queryByTestId('llm-node-output-contract')).not.toBeInTheDocument();
  });
});
