import type { ReactNode } from 'react';
import { memo } from 'react';
import type { NodeProps } from '@xyflow/react';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { ExtensionNodeShell } from '../settings/graph-extensions/ExtensionNodeShell';
import { AggregateSettings } from '../settings/graph-extensions/AggregateSettings';

export const AggregateNode = memo(function AggregateNode(props: NodeProps<FlowNode>): ReactNode {
  return <ExtensionNodeShell {...props} renderSettings={(settings) => <AggregateSettings {...settings} />} />;
});
