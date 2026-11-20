import { memo, useCallback, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { OpenNewIcon } from '@/shared/ui/icons/open-new-icon';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';

import { getAttachmentPagePath } from '../../lib/tool-picker/attachmentTool.helpers';
import { PIPELINE_TOOL_CARD_ACTION_CLASS } from '../../lib/tool-picker/toolPicker.constants';
import type { PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';

export interface PipelineToolCardActionsProps {
  readonly tool: PickerToolEntry;
  readonly entityLabel: string;
  readonly onRemove?: ((tool: PickerToolEntry) => void) | undefined;
}

const rootSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  flexShrink: 0,
});

const iconSx: SxProps<Theme> = {
  width: '1rem',
  height: '1rem',
};

/**
 * Hover actions of a pipeline tool card: open the item in a new tab (`noopener,noreferrer`,
 * no query-string secret/name leaks), and remove it when no node uses it.
 */
export const PipelineToolCardActions = memo(function PipelineToolCardActions({
  tool,
  entityLabel,
  onRemove,
}: PipelineToolCardActionsProps): ReactNode {
  const pagePath = getAttachmentPagePath(tool);

  const handleOpen = useCallback(() => {
    if (!pagePath) return;
    window.open(`${window.location.origin}${pagePath}`, '_blank', 'noopener,noreferrer');
  }, [pagePath]);

  const handleRemove = useCallback(() => {
    onRemove?.(tool);
  }, [onRemove, tool]);

  const openTitle = t('pipelines.toolPicker.openInNewTab', 'Open {{entity}} in new tab', {
    entity: entityLabel,
  });
  const removeTitle = t(
    'pipelines.toolPicker.removeFromPipeline',
    'Remove the {{entity}} from the pipeline',
    { entity: entityLabel },
  );

  return (
    <Box sx={rootSx}>
      {pagePath ? (
        <Tooltip title={openTitle} placement="top">
          <Box component="span" className={PIPELINE_TOOL_CARD_ACTION_CLASS}>
            <BaseBtn
              variant={BUTTON_VARIANTS.tertiary}
              startIcon={<Box component={OpenNewIcon} sx={iconSx} />}
              onClick={handleOpen}
              aria-label={openTitle}
              data-testid="pipeline-tool-card-open"
            />
          </Box>
        </Tooltip>
      ) : null}
      {onRemove ? (
        <Tooltip title={removeTitle} placement="top">
          <Box component="span" className={PIPELINE_TOOL_CARD_ACTION_CLASS}>
            <BaseBtn
              variant={BUTTON_VARIANTS.tertiary}
              startIcon={<Box component={RemoveIcon} sx={iconSx} />}
              onClick={handleRemove}
              aria-label={removeTitle}
              data-testid="pipeline-tool-card-remove"
            />
          </Box>
        </Tooltip>
      ) : null}
    </Box>
  );
});
