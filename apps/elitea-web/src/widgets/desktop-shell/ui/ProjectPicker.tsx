/**
 * The project switcher's popup: a search field over the projects, as the
 * search palette is drawn. It is built for a person in hundreds of projects:
 * typing narrows the list by name, the arrows and Return choose without the
 * mouse, and rows are drawn a page at a time, the next page as the list
 * scrolls near its end, so a long list opens as fast as a short one.
 *
 * The list itself is the one the app already holds (`useProjectOptions`):
 * the server answers every project in one response and takes no search or
 * page parameters, so the narrowing and the paging happen here.
 */
import { useEffect, useMemo, useRef, useState } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import Popover from '@mui/material/Popover';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';

import { MenuRowIcon, MenuRowText, menuRowSx } from '@/shared/ui/native-menu';

/** Rows drawn per page. */
const PROJECT_PAGE = 50;

export interface ProjectPickerProps {
  readonly anchor: HTMLElement | null;
  readonly projects: readonly Project[];
  readonly selectedProjectId: string | undefined;
  readonly onPick: (project: Project) => void;
  readonly onClose: () => void;
}

function matching(projects: readonly Project[], query: string): readonly Project[] {
  const open = projects.filter((project) => !project.suspended);
  const needle = query.trim().toLocaleLowerCase();
  return needle === '' ? open : open.filter((project) => project.name.toLocaleLowerCase().includes(needle));
}

/** Draws the next page when the list's end comes within a row or two of view. */
function useNextPage(more: boolean, onMore: () => void): (node: HTMLElement | null) => void {
  const latest = useRef(onMore);
  useEffect(() => {
    latest.current = onMore;
  });
  const [end, setEnd] = useState<HTMLElement | null>(null);
  useEffect(() => {
    if (!more || end === null || typeof IntersectionObserver === 'undefined') return undefined;
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) latest.current();
    }, { root: end.parentElement, rootMargin: '0px 0px 64px 0px' });
    observer.observe(end);
    return () => observer.disconnect();
  }, [more, end]);
  return setEnd;
}

function PickerList({ projects, selectedProjectId, onPick, onClose }: Omit<ProjectPickerProps, 'anchor'>): React.JSX.Element {
  const [query, setQuery] = useState('');
  // Opens on the current project, its page drawn.
  const [active, setActive] = useState(() => Math.max(0, matching(projects, '').findIndex((project) => String(project.id) === selectedProjectId)));
  const [limit, setLimit] = useState(() => Math.max(PROJECT_PAGE, active + 1));
  const found = useMemo(() => matching(projects, query), [projects, query]);
  const shown = found.slice(0, limit);
  const more = found.length > shown.length;
  const endRef = useNextPage(more, () => setLimit((current) => current + PROJECT_PAGE));
  const listRef = useRef<HTMLUListElement>(null);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => input.current?.focus(), []);
  // Keeps the highlighted row in view by scrolling the list only (as `SuggestionMenu`): `scrollIntoView` scrolls every ancestor too.
  useEffect(() => {
    const list = listRef.current;
    const row = list?.querySelector(`[data-index="${active}"]`);
    if (list === null || list === undefined || !(row instanceof HTMLElement)) return;
    const at = row.getBoundingClientRect();
    const box = list.getBoundingClientRect();
    if (at.top < box.top) list.scrollTop -= box.top - at.top;
    else if (at.bottom > box.bottom) list.scrollTop += at.bottom - box.bottom;
  }, [active]);

  const move = (step: number): void => {
    const next = Math.min(Math.max(active + step, 0), found.length - 1);
    if (next >= limit) setLimit(limit + PROJECT_PAGE);
    setActive(next);
  };
  const onKeyDown = (event: React.KeyboardEvent): void => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      move(event.key === 'ArrowDown' ? 1 : -1);
    } else if (event.key === 'Enter') {
      event.preventDefault();
      const project = found[active];
      if (project !== undefined) onPick(project);
    } else if (event.key === 'Escape') {
      onClose();
    }
  };
  const optionId = (index: number): string => `project-picker-option-${index}`;
  const label = t('desktop.shell.projects', 'Projects');

  return (
    <>
      <InputBase
        inputRef={input}
        fullWidth
        value={query}
        placeholder={t('desktop.shell.searchProjects', 'Search projects')}
        onChange={(event) => {
          setQuery(event.target.value);
          setActive(0);
          setLimit(PROJECT_PAGE);
        }}
        onKeyDown={onKeyDown}
        inputProps={{
          role: 'combobox',
          'aria-label': t('desktop.shell.searchProjects', 'Search projects'),
          'aria-expanded': true,
          'aria-controls': 'project-picker-list',
          'aria-activedescendant': shown.length > 0 ? optionId(active) : undefined,
          'data-testid': 'project-picker-search',
        }}
        sx={(theme: Theme) => ({ ...theme.typography.labelSmall, paddingX: 1.5, paddingY: 1, borderBottom: `1px solid ${theme.vars.palette.divider}` })}
      />
      {/* oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- the combobox pattern: the search field drives this popup; a native <select> cannot */}
      <Box id="project-picker-list" ref={listRef} role="listbox" aria-label={label} component="ul" sx={{ margin: 0, padding: 0.5, listStyle: 'none', maxHeight: '20rem', overflowY: 'auto' }}>
        {shown.length === 0 && (
          <Typography component="li" variant="bodySmall" sx={(theme: Theme) => ({ paddingX: 1, paddingY: 1, color: theme.vars.palette.text.metrics })}>
            {t('desktop.shell.noProjectsFound', 'No projects match')}
          </Typography>
        )}
        {shown.map((project, index) => {
          const current = String(project.id) === selectedProjectId;
          return (
            <Box
              component="li"
              key={project.id}
              id={optionId(index)}
              data-index={index}
              // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- rows of the listbox above; a native <option> needs a <select>
              role="option"
              aria-selected={current}
              onMouseMove={() => setActive(index)}
              onClick={() => onPick(project)}
              sx={(theme: Theme) => ({ ...menuRowSx(theme, index === active), cursor: 'pointer' })}
            >
              <MenuRowText>{project.name}</MenuRowText>
              <MenuRowIcon>{current ? <CheckIcon /> : null}</MenuRowIcon>
            </Box>
          );
        })}
        {more && <Box component="li" aria-hidden ref={endRef} sx={{ height: '1px' }} />}
      </Box>
    </>
  );
}

export function ProjectPicker({ anchor, ...list }: ProjectPickerProps): React.JSX.Element {
  return (
    <Popover
      data-testid="project-picker"
      open={anchor !== null}
      anchorEl={anchor}
      onClose={list.onClose}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      slotProps={{ paper: { sx: (theme: Theme) => ({ width: anchor?.offsetWidth ?? 240, minWidth: '15rem', marginTop: 0.5, borderRadius: theme.vars.shape.radiusMd }) } }}
    >
      {/* Mounted per opening: the query, the page and the highlight start fresh each time. */}
      <PickerList {...list} />
    </Popover>
  );
}
