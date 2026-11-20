import type { MouseEvent, ReactNode } from 'react';
import { memo, useCallback, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { PopoverOrigin } from '@mui/material/Popover';
import Typography from '@mui/material/Typography';
import { useColorScheme, type SxProps, type Theme } from '@mui/material/styles';

import applicationsDarkIllustration from '@/assets/empty-states/applications-dark.webp';
import applicationsLightIllustration from '@/assets/empty-states/applications-light.webp';
import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import type { PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NodeTypeSelector } from '../add-node';

const CENTER_ORIGIN: PopoverOrigin = { vertical: 'center', horizontal: 'center' };

export interface FlowEmptyStateProps {
  readonly onAddNode: (type: PipelineNodeType) => void;
  readonly disabled?: boolean | undefined;
}

const overlaySx: SxProps<Theme> = {
  position: 'absolute',
  inset: 0,
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  pointerEvents: 'none',
  zIndex: 5,
};

const buildCardSx = (isSelectorOpen: boolean): SxProps<Theme> => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: '1rem',
  width: '26.25rem',
  maxWidth: 'calc(100% - 2rem)',
  padding: '2rem 2.5rem 1.5rem',
  boxSizing: 'border-box',
  borderRadius: ({ vars }) => vars.shape.radiusLg,
  border: ({ vars }) => `0.0625rem solid ${vars.palette.border.lines}`,
  backgroundColor: ({ vars }) => vars.palette.background.secondary,
  pointerEvents: 'auto',
  visibility: isSelectorOpen ? 'hidden' : 'visible',
});

const illustrationSx: SxProps<Theme> = {
  width: '12.3125rem',
  height: '7.6875rem',
  objectFit: 'contain',
};

export const FlowEmptyState = memo(function FlowEmptyState({ onAddNode, disabled }: FlowEmptyStateProps): ReactNode {
  const { colorScheme } = useColorScheme();
  const cardRef = useRef<HTMLDivElement | null>(null);
  const overlayRef = useRef<HTMLDivElement | null>(null);
  const [isSelectorOpen, setIsSelectorOpen] = useState(false);

  const handleOpen = useCallback((event: MouseEvent<HTMLElement>) => {
    event.stopPropagation();
    setIsSelectorOpen(true);
  }, []);

  const handleClose = useCallback(() => {
    setIsSelectorOpen(false);
  }, []);

  const illustration = colorScheme === 'dark' ? applicationsDarkIllustration : applicationsLightIllustration;

  return (
    <Box
      ref={overlayRef}
      sx={overlaySx}
    >
      <Box
        ref={cardRef}
        sx={buildCardSx(isSelectorOpen)}
        data-testid="pipeline-empty-state"
      >
        <Box
          component="img"
          src={illustration}
          alt=""
          sx={illustrationSx}
        />
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {t('features.pipelines.emptyState.message', 'Start by adding your first node to the canvas!')}
        </Typography>
        {!disabled && (
          <BaseBtn
            variant={BUTTON_VARIANTS.special}
            startIcon={<PlusIcon />}
            onClick={handleOpen}
            data-testid="pipeline-empty-state-add-node-button"
          >
            {t('features.pipelines.emptyState.addNode', 'Add Node')}
          </BaseBtn>
        )}
      </Box>
      <NodeTypeSelector
        open={isSelectorOpen}
        anchorEl={cardRef.current}
        boundaryEl={overlayRef.current}
        onClose={handleClose}
        onSelect={onAddNode}
        anchorOrigin={CENTER_ORIGIN}
        transformOrigin={CENTER_ORIGIN}
      />
    </Box>
  );
});
