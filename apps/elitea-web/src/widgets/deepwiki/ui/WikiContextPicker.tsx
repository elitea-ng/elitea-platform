/**
 * Attach wiki pages to the next question.
 *
 * WHAT THE READER IS CHOOSING. A question asked with nothing attached is
 * answered from whatever retrieval surfaces; attaching pages says "answer from
 * these". The list offered is the OPEN wiki version's own page list — the
 * manifest's `pages`, the same list the wiki browser renders — because that is
 * the only set the provider will accept. Offering anything else (a repository
 * path, a file the index skipped) would produce a selection the server refuses.
 *
 * IT SENDS IDENTIFIERS, NEVER TEXT. The chips below are labels; what leaves the
 * browser is the page id. The page bodies are resolved server-side, from this
 * project's own artifacts, so a hostile client cannot use this control to make
 * the server read something else — and a large page costs the reader nothing to
 * attach, because the budget is applied there too.
 */
import { memo, useCallback, useMemo, useState, type MouseEvent } from 'react';

import AttachFileIcon from '@mui/icons-material/AttachFile';
import Badge from '@mui/material/Badge';
import Checkbox from '@mui/material/Checkbox';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

export interface WikiContextPickerProps {
  /** The open version's page ids, as the manifest lists them. */
  readonly pages: readonly string[];
  /** The ids currently attached, in the order they were chosen. */
  readonly selected: readonly string[];
  readonly onChange: (selected: readonly string[]) => void;
  readonly disabled: boolean;
}

/**
 * The label for a page id.
 *
 * The manifest carries no titles — `WikiPageView` derives its heading the same
 * way. The section is kept because two sections routinely hold a page of the
 * same name ("overview/index.md", "api/index.md") and a bare leaf would show
 * the reader two identical rows.
 */
function pageLabel(pageId: string): string {
  const parts = pageId.replace(/\.md$/, '').split('/');
  return parts.slice(-2).join(' / ') || pageId;
}

export const WikiContextPicker = memo(function WikiContextPicker({
  pages,
  selected,
  onChange,
  disabled,
}: WikiContextPickerProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);

  const chosen = useMemo(() => new Set(selected), [selected]);

  const toggle = useCallback(
    (pageId: string) => {
      // Order is the reader's selection order, and it is meaningful: the
      // server renders the block in the order it receives, and truncates from
      // the end when the budget runs out. Re-sorting here would silently
      // change which page survives a long selection.
      onChange(chosen.has(pageId) ? selected.filter((id) => id !== pageId) : [...selected, pageId]);
    },
    [chosen, onChange, selected],
  );

  const open = useCallback((event: MouseEvent<HTMLElement>) => {
    setAnchor(event.currentTarget);
  }, []);
  const close = useCallback(() => {
    setAnchor(null);
  }, []);

  if (pages.length === 0) return null;

  const label = t('widgets.deepwiki.chat.attachPages', 'Attach wiki pages');

  return (
    <>
      <Tooltip title={label}>
        <span>
          <IconButton
            size="small"
            onClick={open}
            disabled={disabled}
            aria-label={label}
            data-testid="wiki-chat-context-button"
          >
            <Badge badgeContent={selected.length} color="primary">
              <AttachFileIcon fontSize="small" />
            </Badge>
          </IconButton>
        </span>
      </Tooltip>

      <Menu
        open={anchor !== null}
        anchorEl={anchor}
        onClose={close}
        // No testid on the menu itself: MUI types both the list and the paper
        // slot props without an index signature for data attributes. The
        // options carry their own, which is what a test actually clicks.
      >
        {pages.map((pageId) => (
          <MenuItem
            key={pageId}
            onClick={() => {
              toggle(pageId);
            }}
            data-testid="wiki-chat-context-option"
          >
            <Checkbox size="small" checked={chosen.has(pageId)} tabIndex={-1} disableRipple />
            <ListItemText primary={pageLabel(pageId)} secondary={pageId} />
          </MenuItem>
        ))}
      </Menu>

      {selected.length > 0 ? (
        <Stack
          sx={{ flexDirection: 'row', flexWrap: 'wrap', gap: 0.5, width: '100%' }}
          data-testid="wiki-chat-context-chips"
        >
          {selected.map((pageId) => (
            <Chip
              key={pageId}
              size="small"
              label={pageLabel(pageId)}
              onDelete={
                disabled
                  ? undefined
                  : () => {
                      toggle(pageId);
                    }
              }
            />
          ))}
        </Stack>
      ) : null}
    </>
  );
});
