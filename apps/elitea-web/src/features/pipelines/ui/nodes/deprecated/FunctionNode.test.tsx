import { describe, expect, it, vi } from 'vitest';

import type { FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { renderDeprecatedNode } from './deprecatedNodeTestUtils';
import { FunctionNode, isNotApplicationTool } from './FunctionNode';

describe('isNotApplicationTool', () => {
  it('excludes application-type ("agent"/pipeline) associations', () => {
    expect(isNotApplicationTool({ type: 'application', name: 'sub-agent' })).toBe(false);
  });

  it('includes every other toolkit type', () => {
    expect(isNotApplicationTool({ type: 'github', toolkit_name: 'my-github' })).toBe(true);
  });
});

describe('FunctionNode', () => {
  it('renders the node card with the toolkit picker, without crashing, forwarding versionTools/id through to BaseToolNode', async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const contextValue: FlowEditorContextValue = {
      yamlJsonObject: { nodes: [{ id: 'function-1', type: 'function' }] },
      setYamlJsonObject: vi.fn(),
      setFlowNodes: vi.fn(),
      setFlowEdges: vi.fn(),
    };

    const { findByTestId } = renderDeprecatedNode(
      'function-1',
      contextValue,
      <FunctionNode
        id="function-1"
        data={{}}
        versionTools={versionTools}
      />,
    );

    expect(await findByTestId('pipeline-node-v2-name')).toHaveTextContent('function-1');
  });
});
