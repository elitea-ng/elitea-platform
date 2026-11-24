import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { PipelineNodeTypes } from '../../../lib/flow-editor/constants/flowEditor.constants';
import { InputMappingFields } from './InputMappingFields';

const FLOW_CONTEXT = {
  yamlJsonObject: { state: { input: 'str' } },
  setFlowNodes: vi.fn(),
  setFlowEdges: vi.fn(),
  setYamlJsonObject: vi.fn(),
};

describe('InputMappingFields defensive lookups and metadata narrowing', () => {
  it('safely handles reserved prototype property names without reading Object.prototype', () => {
    renderWithTheme(
      <FlowEditorContext.Provider value={FLOW_CONTEXT}>
        <InputMappingFields
          keys={['toString', 'constructor']}
          input_mapping={{
            toString: { type: 'fixed', value: '' },
            constructor: { type: 'fixed', value: '' },
          }}
          values={{}}
          mappingInfo={{}}
          defaultValues={{}}
          onChangeMapping={vi.fn()}
          nodeType={PipelineNodeTypes.Mcp}
        />
      </FlowEditorContext.Provider>,
    );

    expect(screen.getByText('ToString')).toBeInTheDocument();
    expect(screen.getByText('Constructor')).toBeInTheDocument();
  });

  it('degrades non-string schema title and tooltip metadata safely to fallback strings', () => {
    renderWithTheme(
      <FlowEditorContext.Provider value={FLOW_CONTEXT}>
        <InputMappingFields
          keys={['query_text']}
          input_mapping={{
            query_text: { type: 'fixed', value: 'hello', title: { nested: 1 } as unknown as string },
          }}
          values={{}}
          mappingInfo={{
            query_text: { type: 'fixed', value: '', tooltip: { malicious: true } as unknown as string },
          }}
          defaultValues={{}}
          onChangeMapping={vi.fn()}
          nodeType={PipelineNodeTypes.Mcp}
        />
      </FlowEditorContext.Provider>,
    );

    expect(screen.getByText('Query text')).toBeInTheDocument();
  });
});
