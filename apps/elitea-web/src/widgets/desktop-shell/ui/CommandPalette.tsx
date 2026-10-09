/**
 * ⌘K: one box to reach anything — a folder or thread, an action (new thread,
 * open folder, plan mode, panes, settings) or an Elitea page. Typing filters
 * (every word must match), ↑/↓ move, Enter runs, Esc closes.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';

import { useQuery } from '@tanstack/react-query';

import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { readThreads, useWorkspaceIpc } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { EliteaItem } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { WORKSPACE_LIST_KEY } from '../model/useShellActions';
import { modKey } from './shortcutLabel';

export interface PaletteEntry {
  id: string;
  label: string;
  group: string;
  hint?: string;
  action: ShellAction;
}

export function filterEntries(entries: readonly PaletteEntry[], query: string): PaletteEntry[] {
  const words = query.toLowerCase().split(/\s+/).filter((word) => word !== '');
  if (words.length === 0) return [...entries];
  return entries.filter((entry) => {
    const haystack = `${entry.label} ${entry.group} ${entry.hint ?? ''}`.toLowerCase();
    return words.every((word) => haystack.includes(word));
  });
}

function paletteEntries(workspaces: readonly Workspace[], eliteaItems: readonly EliteaItem[], hasSession: boolean): PaletteEntry[] {
  const mod = modKey();
  const actions = t('desktop.palette.actions', 'Actions');
  const folders = t('desktop.palette.folders', 'Folders');
  const threads = t('desktop.palette.threads', 'Threads');
  const elitea = t('desktop.palette.elitea', 'Elitea');
  const entries: PaletteEntry[] = [
    { id: 'new_thread', group: actions, label: t('desktop.palette.newThread', 'New thread'), hint: `${mod}N`, action: { type: 'new_thread' } },
    { id: 'open_folder', group: actions, label: t('desktop.palette.openFolder', 'Open folder…'), hint: `${mod}O`, action: { type: 'open_folder' } },
  ];
  if (hasSession) {
    entries.push({ id: 'toggle_plan', group: actions, label: t('desktop.palette.togglePlan', 'Toggle plan mode'), action: { type: 'toggle_plan' } });
  }
  entries.push(
    { id: 'toggle_sidebar', group: actions, label: t('desktop.palette.toggleSidebar', 'Toggle sidebar'), hint: `${mod}\\`, action: { type: 'toggle_sidebar' } },
    {
      id: 'toggle_changes',
      group: actions,
      label: t('desktop.palette.toggleChanges', 'Toggle changes panel'),
      hint: `${mod}⌥\\`,
      action: { type: 'toggle_changes' },
    },
    { id: 'settings', group: actions, label: t('desktop.palette.settings', 'Settings'), hint: `${mod},`, action: { type: 'settings' } },
  );
  for (const workspace of workspaces) {
    entries.push({ id: `folder:${workspace.id}`, group: folders, label: workspace.name, hint: workspace.path, action: { type: 'open_workspace', workspaceId: workspace.id } });
  }
  for (const workspace of workspaces) {
    for (const thread of readThreads(workspace.id)) {
      entries.push({
        id: `thread:${workspace.id}:${thread.id}`,
        group: threads,
        label: thread.title === '' ? t('desktop.shell.untitledThread', 'Untitled thread') : thread.title,
        hint: workspace.name,
        action: { type: 'open_workspace', workspaceId: workspace.id, conversationId: thread.id },
      });
    }
  }
  for (const item of eliteaItems) {
    entries.push({ id: `go:${item.value}`, group: elitea, label: item.label, action: { type: 'go', to: item.url } });
  }
  return entries;
}

function PaletteBody({ items, run }: { items: readonly EliteaItem[]; run: (action: ShellAction) => void }): React.JSX.Element {
  const ipc = useWorkspaceIpc();
  const list = useQuery({ queryKey: WORKSPACE_LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const hasSession = useDesktopLayout((state) => state.session !== null);
  const input = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const entries = useMemo(() => paletteEntries(list.data ?? [], items, hasSession), [list.data, items, hasSession]);
  const shown = useMemo(() => filterEntries(entries, query), [entries, query]);
  const current = Math.min(active, Math.max(shown.length - 1, 0));

  const choose = (entry: PaletteEntry | undefined): void => {
    if (entry === undefined) return;
    useDesktopLayout.getState().setPaletteOpen(false);
    run(entry.action);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      setActive((current + 1) % Math.max(shown.length, 1));
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      setActive((current - 1 + shown.length) % Math.max(shown.length, 1));
    } else if (event.key === 'Enter') {
      event.preventDefault();
      choose(shown[current]);
    }
  };

  const optionId = (index: number): string => `palette-option-${String(index)}`;
  // The query box takes the keyboard as the palette opens (the body mounts with it).
  useEffect(() => input.current?.focus(), []);

  return (
    <>
      <InputBase
        inputRef={input}
        fullWidth
        value={query}
        placeholder={t('desktop.palette.placeholder', 'Search folders, threads and commands')}
        onChange={(event) => {
          setQuery(event.target.value);
          setActive(0);
        }}
        onKeyDown={onKeyDown}
        inputProps={{
          role: 'combobox',
          'aria-expanded': true,
          'aria-controls': 'palette-list',
          'aria-activedescendant': shown.length > 0 ? optionId(current) : undefined,
          'aria-label': t('desktop.palette.label', 'Command palette'),
        }}
        sx={(theme: Theme) => ({ paddingX: 2, paddingY: 1.5, borderBottom: `1px solid ${theme.vars.palette.divider}` })}
      />
      {/* oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- the combobox pattern: the query box drives this popup; a native <select> cannot */}
      <Box id="palette-list" role="listbox" component="ul" sx={{ margin: 0, padding: 0.5, listStyle: 'none', maxHeight: '22rem', overflowY: 'auto' }}>
        {shown.length === 0 && (
          <Typography component="li" variant="bodySmall" sx={(theme: Theme) => ({ padding: 1.5, color: theme.vars.palette.text.metrics })}>
            {t('desktop.palette.empty', 'Nothing matches')}
          </Typography>
        )}
        {shown.map((entry, index) => (
          <Box
            component="li"
            key={entry.id}
            id={optionId(index)}
            // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- rows of the listbox above; a native <option> needs a <select>
            role="option"
            aria-selected={index === current}
            onMouseMove={() => setActive(index)}
            onClick={() => choose(entry)}
            sx={(theme: Theme) => ({
              display: 'flex',
              alignItems: 'center',
              gap: 1.5,
              paddingX: 1.5,
              paddingY: 0.75,
              borderRadius: theme.vars.shape.radiusSm,
              cursor: 'pointer',
              background: index === current ? theme.vars.palette.background.button.drawerMenu.selected : undefined,
            })}
          >
            <Typography variant="labelSmall" component="span" sx={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {entry.label}
            </Typography>
            {entry.hint !== undefined && (
              <Typography
                variant="bodySmall"
                component="span"
                sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, maxWidth: '45%', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}
              >
                {entry.hint}
              </Typography>
            )}
            <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
              {entry.group}
            </Typography>
          </Box>
        ))}
      </Box>
    </>
  );
}

export function CommandPalette({ items, run }: { items: readonly EliteaItem[]; run: (action: ShellAction) => void }): React.JSX.Element {
  const open = useDesktopLayout((state) => state.paletteOpen);
  return (
    <Dialog
      open={open}
      onClose={() => useDesktopLayout.getState().setPaletteOpen(false)}
      fullWidth
      maxWidth="sm"
      aria-label={t('desktop.palette.label', 'Command palette')}
      slotProps={{ container: { sx: { alignItems: 'flex-start' } }, paper: { sx: { marginTop: '12vh' } } }}
    >
      {/* Remounted on every open: a fresh query and selection each time. */}
      {open && <PaletteBody items={items} run={run} />}
    </Dialog>
  );
}
