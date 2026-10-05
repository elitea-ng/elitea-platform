import { memo, useCallback, useRef, useState } from 'react';

import { t } from '@/shared/i18n';

import ClickAwayListener from '@mui/material/ClickAwayListener';
import SettingsIcon from '@mui/icons-material/Settings';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import IconButton from '@mui/material/IconButton';
import ListItemText from '@mui/material/ListItemText';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

/**
 * Chat button primitive: ChatInternalToolsConfigButton
 *
 * Renders a settings / gear icon button that opens a popper with toggles for
 * internal chat tools (e.g. web search, code interpreter, file upload).
 * Each toggle is a checkbox that calls `onToolChange` when switched.
 *
 * Purely prop-driven — it does NOT call `useAvailableInternalTools()`
 * itself (unlike baseline `ChatInternalToolsConfigButton.jsx`, which reads
 * it directly): the composition root that eventually wires this into
 * `ChatBox` supplies real `tools`/`onToolChange`, matching whatever data
 * shape it has resolved for its own use. Until it does, `tools` is
 * `undefined` (the button's only current caller passes none) and this
 * component falls back to `DEV_FALLBACK_TOOLS` — a dev-only stand-in, not
 * real product data (see that constant's own doc comment). Renders nothing
 * when the resolved tool list is empty, matching baseline's own
 * hide-when-empty behavior.
 *
 * Prop contract (injected by the composition root through `slots.internalToolsConfig`):
 *   - `disabled`     — disable the settings button
 *   - `tools`        — the real tool list to render (omit entirely for the dev fallback; pass `[]` to hide the button)
 *   - `onToolChange` — fired with `(toolKey, nextEnabled)` when a toggle is clicked
 */
export interface ChatInternalToolsConfigButtonProps {
  disabled?: boolean;
  /** Tool toggle configuration — key/value pairs that the consumer can persist. */
  tools?: { key: string; label: string; enabled: boolean }[];
  /** Callback invoked when a tool toggle changes. */
  onToolChange?: (toolKey: string, enabled: boolean) => void;
}

/**
 * Fallback-only, dev-time tool list — NOT the primary data source. Applies
 * only when the caller supplies no `tools` prop at all (`undefined`, not an
 * explicit `[]`). Real tool data comes from `useAvailableInternalTools()`
 * (`features/agents/lib/internalTools.ts`) via whatever `tools`/
 * `onToolChange` props a future composition-root stage passes in — wiring
 * that INTO this component from ChatBox is that later stage's job, not
 * this one's (see this file's module doc).
 */
const DEV_FALLBACK_TOOLS: { key: string; label: string }[] = [
  { key: 'web_search', label: 'Web Search' },
  { key: 'code_interpreter', label: 'Code Interpreter' },
  { key: 'file_upload', label: 'File Upload' },
  { key: 'image_generation', label: 'Image Generation' },
];

export const ChatInternalToolsConfigButton = memo(
  ({ disabled = false, tools, onToolChange }: ChatInternalToolsConfigButtonProps) => {
    const [anchorEl, setAnchorEl] = useState<HTMLButtonElement | null>(null);
    const buttonRef = useRef<HTMLButtonElement>(null);

    const isOpen = anchorEl !== null;

    const toggleMenu = useCallback(() => {
      setAnchorEl((prev) => (prev ? null : buttonRef.current));
    }, []);

    const closeMenu = useCallback(() => {
      setAnchorEl(null);
    }, []);

    // `tools === undefined` (no prop supplied at all) -> dev fallback. An
    // explicit `tools={[]}` is real data saying "no tools available" and
    // must NOT be overridden by the fallback — it hits the hide-when-empty
    // return below instead, matching baseline's own hide-when-empty behavior.
    const resolvedTools = tools ?? DEV_FALLBACK_TOOLS.map((t) => ({ ...t, enabled: true }));

    const handleToolToggle = useCallback(
      (toolKey: string, enabled: boolean) => {
        onToolChange?.(toolKey, enabled);
      },
      [onToolChange],
    );

    // Baseline (`ChatInternalToolsConfigButton.jsx:40-42`): don't render the
    // button at all when no tools are available.
    if (resolvedTools.length === 0) return null;

    return (
      <>
        <Tooltip title={t('widgets.chat.chatInternalToolsConfigButton.tooltip', 'Tools')} placement="top">
          <IconButton
            ref={buttonRef}
            color="secondary"
            aria-label={t('widgets.chat.chatInternalToolsConfigButton.ariaLabel', 'internal tools config')}
            onClick={toggleMenu}
            disabled={disabled}
            sx={{ marginLeft: 0 }}
          >
            <SettingsIcon fontSize="small" />
          </IconButton>
        </Tooltip>

        <Popper
          open={isOpen}
          anchorEl={anchorEl}
          placement="top-end"
          sx={{ zIndex: 9998 }}
        >
          <ClickAwayListener onClickAway={closeMenu}>
            <Paper
              elevation={8}
              sx={(theme: Theme) => ({
                minWidth: '14rem',
                borderRadius: theme.vars.shape.radiusMd,
                border: '0.0625rem solid',
                borderColor: 'border.lines',
                boxShadow: theme.vars.palette.boxShadow.default,
                background: theme.vars.palette.background.secondary,
                padding: '0.25rem 0',
              })}
            >
              <MenuList sx={{ padding: 0 }}>
                {resolvedTools.map((tool) => (
                  <MenuItem
                    key={tool.key}
                    onClick={() => handleToolToggle(tool.key, !tool.enabled)}
                    sx={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 1,
                      padding: '0.375rem 1rem',
                      height: '2.25rem',
                      '&:hover': {
                        backgroundColor: 'action.hover',
                      },
                    }}
                  >
                    <BaseCheckbox
                      size="small"
                      checked={tool.enabled}
                      onChange={() => handleToolToggle(tool.key, !tool.enabled)}
                      onClick={(e) => e.stopPropagation()}
                    />
                    <ListItemText
                      primary={
                        <Typography
                          variant="bodySmall"
                          sx={{
                            lineHeight: '1.375rem',
                            color: 'text.secondary',
                          }}
                        >
                          {tool.label}
                        </Typography>
                      }
                    />
                  </MenuItem>
                ))}
              </MenuList>
            </Paper>
          </ClickAwayListener>
        </Popper>
      </>
    );
  },
);

ChatInternalToolsConfigButton.displayName = 'ChatInternalToolsConfigButton';
