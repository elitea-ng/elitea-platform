import { useEffect, type ReactNode } from 'react';

import { useDraggable } from '@dnd-kit/core';
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import { entityDragId, useEntityFolderDnd } from '../lib/entityDnd';

interface DragHandleProps {
  readonly entityId: number;
  /** The row's entity name: the handle's accessible name, the overlay and what a screen reader says. */
  readonly entityName: string;
}

/**
 * The grip on a card or a table row: grab it with the pointer, or focus it and
 * press Space to pick the entity up, the arrow keys to choose a folder in the
 * sidebar and Space to drop it. It draws nothing where the page has no drag and
 * drop (no update rights on the list), so the row keeps one control fewer.
 */
export function DragHandle({ entityId, entityName }: DragHandleProps): ReactNode {
  const dnd = useEntityFolderDnd();
  if (dnd === undefined) return null;
  return (
    <ActiveHandle
      entityId={entityId}
      entityName={entityName}
      register={dnd.register}
    />
  );
}

function ActiveHandle({ entityId, entityName, register }: DragHandleProps & { readonly register: (entityId: number, name: string) => void }): ReactNode {
  const { attributes, listeners, setNodeRef, isDragging } = useDraggable({ id: entityDragId(entityId) });
  useEffect(() => {
    register(entityId, entityName);
  }, [register, entityId, entityName]);
  return (
    <Tooltip title={t('widgets.entityFolders.dnd.handle', 'Drag to a folder')}>
      <IconButton
        ref={setNodeRef}
        size="small"
        data-testid="entity-drag-handle"
        aria-label={t('widgets.entityFolders.dnd.handleLabel', 'Drag {{name}} to a folder', { name: entityName })}
        onClick={(event) => {
          // The handle sits inside a clickable card or row: a click on it never opens that row.
          event.stopPropagation();
        }}
        // `touch-action: none` lets a finger drag the handle instead of scrolling the page.
        sx={{ touchAction: 'none', cursor: isDragging ? 'grabbing' : 'grab', opacity: isDragging ? 0.4 : 1 }}
        {...attributes}
        {...listeners}
      >
        <DragIndicatorIcon fontSize="small" />
      </IconButton>
    </Tooltip>
  );
}
