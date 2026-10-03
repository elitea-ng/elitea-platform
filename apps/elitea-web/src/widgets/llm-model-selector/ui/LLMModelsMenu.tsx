import { memo } from 'react';

import { Box, ListItemIcon, MenuItem, Menu, Typography } from '@mui/material';
import CheckCircleOutlineIcon from '@mui/icons-material/CheckCircleOutlineOutlined';
import BusinessIcon from '@mui/icons-material/Business';
import PublicIcon from '@mui/icons-material/Public';

import type { LLMModel } from '@/widgets/llm-model-selector/lib/types';
import { CapabilityChip } from './settings/CapabilityChip';

interface LLMModelsMenuProps {
  anchorEl: null | HTMLElement;
  onClose: () => void;
  models: LLMModel[];
  selectedModel?: LLMModel | null;
  onSelectModel: (model: LLMModel) => void;
}

/**
 * Dropdown menu listing available LLM models.
 * Ported from `[fsd]/widgets/llm-model-selector/ui/LLMModelsMenu.jsx`.
 */
const LLMModelsMenu = memo(
  ({ anchorEl, onClose, models, selectedModel, onSelectModel }: LLMModelsMenuProps) => {
    const open = Boolean(anchorEl);

    const handleItemClick = (model: LLMModel) => () => {
      onSelectModel(model);
      onClose();
    };

    return (
      <Menu
        anchorEl={anchorEl}
        open={open}
        onClose={onClose}
        anchorOrigin={{
          vertical: 'top',
          horizontal: 'right',
        }}
        transformOrigin={{
          vertical: 'bottom',
          horizontal: 'right',
        }}
        slotProps={{
          paper: {
            sx: {
              marginTop: '-0.25rem',
              width: 332,
            },
          },
          list: {
            'aria-labelledby': 'model-selector-button',
          },
        }}
      >
        {models.map((item) => (
          <MenuItem
            key={item.id}
            selected={item.id === selectedModel?.id}
            onClick={handleItemClick(item)}
            sx={{
              '&:hover': {
                backgroundColor: 'action.hover',
              },
              '&.Mui-selected': {
                backgroundColor: 'action.selected',
              },
              '&.Mui-selected:hover': {
                backgroundColor: 'action.selected',
              },
            }}
          >
            <ListItemIcon sx={{ minWidth: 0, marginRight: '0.6rem' }}>
              {item.shared ? (
                <PublicIcon fontSize="small" />
              ) : (
                <BusinessIcon fontSize="small" />
              )}
            </ListItemIcon>
            <Box sx={{ display: 'flex', alignItems: 'center', flex: 1, minWidth: 0, width: '100%' }}>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0, overflow: 'hidden' }}>
                <Typography
                  variant="bodyMedium"
                  component="p"
                  sx={{
                    flex: 1,
                    minWidth: 0,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {item.display_name || item.name}
                </Typography>
                {(item.supports_vision || item.supports_reasoning) && (
                  <Box sx={{ display: 'flex', gap: 0.25, flexShrink: 0 }}>
                    {item.supports_vision && <CapabilityChip type="vision" showTooltip />}
                    {item.supports_reasoning && <CapabilityChip type="reasoning" showTooltip />}
                  </Box>
                )}
              </Box>
              {item.id === selectedModel?.id && (
                <Box sx={{ display: 'flex', alignItems: 'center', marginLeft: 'auto' }}>
                  <CheckCircleOutlineIcon
                    fontSize="small"
                    sx={{ width: '1.125rem', height: '1.125rem', color: 'text.secondary', marginLeft: '1rem' }}
                  />
                </Box>
              )}
            </Box>
          </MenuItem>
        ))}
      </Menu>
    );
  },
);

LLMModelsMenu.displayName = 'LLMModelsMenu';

export default LLMModelsMenu;
