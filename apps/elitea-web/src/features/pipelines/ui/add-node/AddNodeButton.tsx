import type { MouseEvent, ReactNode } from 'react';
import { memo, useCallback, useState } from 'react';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import type { PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NodeTypeSelector } from './NodeTypeSelector';

export interface AddNodeButtonProps {
  readonly onAddNode: (type: PipelineNodeType) => void;
  readonly disabled?: boolean | undefined;
  readonly boundaryEl?: HTMLElement | null | undefined;
}

export const AddNodeButton = memo(function AddNodeButton({
  onAddNode,
  disabled,
  boundaryEl,
}: AddNodeButtonProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const open = Boolean(anchorEl);

  const handleOpen = useCallback((event: MouseEvent<HTMLElement>) => {
    event.stopPropagation();
    setAnchorEl(event.currentTarget);
  }, []);

  const handleClose = useCallback(() => {
    setAnchorEl(null);
  }, []);

  return (
    <>
      <BaseBtn
        id="pipeline-add-node-menu-action"
        data-testid="pipeline-add-node-button"
        variant={BUTTON_VARIANTS.iconLabel}
        startIcon={<PlusIcon />}
        aria-label={t('features.pipelines.editorPanel.addNodeMenu.ariaLabel', 'Add node')}
        aria-controls={open ? 'pipeline-add-node-menu' : undefined}
        aria-haspopup="true"
        aria-expanded={open ? 'true' : undefined}
        onClick={handleOpen}
        disabled={disabled ?? false}
      >
        {t('features.pipelines.addNode.buttonLabel', 'Node')}
      </BaseBtn>
      <NodeTypeSelector
        open={open}
        anchorEl={anchorEl}
        boundaryEl={boundaryEl}
        onClose={handleClose}
        onSelect={onAddNode}
      />
    </>
  );
});
