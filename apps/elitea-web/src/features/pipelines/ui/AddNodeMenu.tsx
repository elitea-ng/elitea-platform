import type { ReactNode } from 'react';
import { memo } from 'react';

import type { PipelineNodeType } from '../lib/flow-editor/constants/flowEditor.constants';
import { AddNodeButton } from './add-node';

export interface AddNodeMenuProps {
  readonly onAddNode: (type: PipelineNodeType) => void;
  readonly disabled?: boolean | undefined;
  readonly boundaryEl?: HTMLElement | null | undefined;
}

/**
 * Toolbar "Node" trigger + responsive 1–2 column `NodeTypeSelector` palette.
 * Preserves the `AddNodeMenu` symbol for existing callers (`EditorPanel` and tests).
 */
export const AddNodeMenu = memo(function AddNodeMenu({
  onAddNode,
  disabled,
  boundaryEl,
}: AddNodeMenuProps): ReactNode {
  return (
    <AddNodeButton
      onAddNode={onAddNode}
      disabled={disabled}
      boundaryEl={boundaryEl}
    />
  );
});
