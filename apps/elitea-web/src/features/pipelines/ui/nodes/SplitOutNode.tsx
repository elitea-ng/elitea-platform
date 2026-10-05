import type { ReactNode } from 'react';
import { memo } from 'react';
import type { NodeProps } from '@xyflow/react';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { ExtensionNodeShell } from '../settings/graph-extensions/ExtensionNodeShell';
import { SplitOutSettings } from '../settings/graph-extensions/SplitOutSettings';

export const SplitOutNode = memo(function SplitOutNode(props: NodeProps<FlowNode>): ReactNode {
  return <ExtensionNodeShell {...props} renderSettings={(settings) => <SplitOutSettings {...settings} />} />;
});
