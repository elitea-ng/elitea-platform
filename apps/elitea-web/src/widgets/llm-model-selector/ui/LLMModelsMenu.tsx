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
  /** The id of the trigger button that names this list. */
  labelledBy?: string;
}

/**
 * The model description, or `undefined` for none. Read defensively: some
 * callers pass raw catalogue rows rather than `toLlmModel` output, so the
 * value is not guaranteed to be a trimmed string.
 */
function descriptionOf(item: LLMModel): string | undefined {
  const value: unknown = item.description;
  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  return trimmed === '' ? undefined : trimmed;
}

/**
 * Dropdown menu listing available LLM models.
 * Ported from `[fsd]/widgets/llm-model-selector/ui/LLMModelsMenu.jsx`.
 *
 * Each row shows the model name and, when the admin wrote one, the model's
 * description as a smaller grey second line (legacy issues 6766 and 6727). A
 * row without a description keeps its old single-line height. The list is a
 * listbox of options; Esc and a click outside close it (MUI Menu).
 */
const LLMModelsMenu = memo(
  ({ anchorEl, onClose, models, selectedModel, onSelectModel, labelledBy }: LLMModelsMenuProps) => {
    const open = Boolean(anchorEl);
    // A named object, not an inline literal: MUI's slot type has no `data-*`
    // keys, and only a literal is checked for excess properties.
    const listSlotProps = {
      role: 'listbox',
      'data-testid': 'model-selector-listbox',
      ...(labelledBy !== undefined ? { 'aria-labelledby': labelledBy } : {}),
    };

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
          list: listSlotProps,
        }}
      >
        {models.map((item) => {
          const selected = item.id === selectedModel?.id;
          const description = descriptionOf(item);
          return (
            <MenuItem
              key={item.id}
              // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- MUI MenuItem in a listbox; a native <option> cannot hold this row's layout
              role="option"
              aria-selected={selected}
              selected={selected}
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
                {item.shared ? <PublicIcon fontSize="small" /> : <BusinessIcon fontSize="small" />}
              </ListItemIcon>
              <Box sx={{ display: 'flex', alignItems: 'center', flex: 1, minWidth: 0, width: '100%' }}>
                <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0, overflow: 'hidden' }}>
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
                  {description !== undefined && (
                    <Typography
                      variant="bodySmall"
                      component="p"
                      color="text.secondary"
                      data-testid="model-option-description"
                      // Never cut off: a description is at most 40
                      // characters, sized for one line. When a brand font
                      // is wider, the line wraps instead of hiding its end.
                      sx={{ whiteSpace: 'normal', overflowWrap: 'anywhere' }}
                    >
                      {description}
                    </Typography>
                  )}
                </Box>
                {selected && (
                  <Box sx={{ display: 'flex', alignItems: 'center', marginLeft: 'auto' }}>
                    <CheckCircleOutlineIcon
                      fontSize="small"
                      sx={{ width: '1.125rem', height: '1.125rem', color: 'text.secondary', marginLeft: '1rem' }}
                    />
                  </Box>
                )}
              </Box>
            </MenuItem>
          );
        })}
      </Menu>
    );
  },
);

LLMModelsMenu.displayName = 'LLMModelsMenu';

export default LLMModelsMenu;
