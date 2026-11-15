import { useCallback, useEffect, useRef, type ReactNode } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { SidePanelTab, useSidePanelSlot } from '@/shared/ui/side-panel';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import { FacetGroup, ModeSwitch, Section, SortControl } from './ListSidebarControls';
import { ListSummaryBar } from './ListSummaryBar';
import { ListViewToggle } from './ListViewToggle';
import { useListSearch, type EntityListSidebarProps } from './listSidebarModel';
import { useListSearchQuery } from './PageSearch';

/**
 * The Workshop's right sidebar (ADR-0051 decision 7): a List tab holding every
 * control of the list page — search, New, Import, the facets, sort, the
 * card/table switch and the folders — so the page itself has no toolbar row.
 * It renders the page's one summary bar as well: when the sidebar is closed,
 * or is a drawer over the page, a single line above the list keeps the
 * search text and the active filters on screen, with Clear, and New.
 *
 * `/` or ⌘F (Ctrl+F) focuses the search, opening the sidebar first when it
 * is closed.
 */
export function EntityListSidebar(props: EntityListSidebarProps): ReactNode {
  const slot = useSidePanelSlot();
  const { focusSearch, registerSearch } = useSearchShortcut();
  const listVisible = slot?.targets.has('list') === true;
  const showSummary = slot === null || !listVisible || slot.overlay === true;
  return (
    <>
      <SidePanelTab
        id="list"
        title={t('shell.sidePanel.list', 'List')}
        subject={props.subject}
      >
        <SidebarBody
          {...props}
          registerSearch={registerSearch}
        />
      </SidePanelTab>
      {showSummary && (
        <ListSummaryBar
          {...props}
          onOpenSidebar={focusSearch}
        />
      )}
    </>
  );
}

/**
 * `/` and ⌘F open the sidebar and focus its search. The box registers itself
 * while it is mounted; with the sidebar closed there is no box, so the shortcut
 * opens the tab first and focuses the search once it mounts.
 */
function useSearchShortcut(): { readonly focusSearch: () => void; readonly registerSearch: (node: HTMLInputElement | null) => void } {
  const slot = useSidePanelSlot();
  const input = useRef<HTMLInputElement | null>(null);
  const focusWhenMounted = useRef(false);

  const focusSearch = useCallback(() => {
    if (input.current !== null) {
      input.current.focus();
      input.current.select();
      return;
    }
    focusWhenMounted.current = true;
    slot?.show('list');
  }, [slot]);

  const registerSearch = useCallback((node: HTMLInputElement | null) => {
    input.current = node;
    if (node === null || !focusWhenMounted.current) return;
    focusWhenMounted.current = false;
    node.focus();
  }, []);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.defaultPrevented) return;
      const find = (event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === 'f';
      const slash = event.key === '/' && !event.altKey && !event.metaKey && !event.ctrlKey && !isTextField(event.target);
      if (!find && !slash) return;
      event.preventDefault();
      focusSearch();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [focusSearch]);

  return { focusSearch, registerSearch };
}

function isTextField(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return target.isContentEditable || target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.tagName === 'SELECT';
}

type SidebarBodyProps = EntityListSidebarProps & { readonly registerSearch: (node: HTMLInputElement | null) => void };

function SidebarBody(props: SidebarBodyProps): ReactNode {
  const { testIdPrefix, modes, listControls = true, folders, filters, selection } = props;
  return (
    <Box
      data-testid={`${testIdPrefix}-list-sidebar`}
      sx={bodySx}
    >
      {selection}
      {modes !== undefined && (
        <ModeSwitch
          modes={modes}
          label={t('widgets.listSidebar.modes', 'Show')}
        />
      )}
      {listControls && <SidebarSearch {...props} />}
      <SidebarActions {...props} />
      {listControls && <SidebarFilters {...props} />}
      {listControls && folders}
      {listControls && filters}
    </Box>
  );
}

/** The search box. It hands its input to the shortcut, so `/` and ⌘F can focus it. */
function SidebarSearch({ testIdPrefix, searchPlaceholder, registerSearch }: SidebarBodyProps): ReactNode {
  const query = useListSearchQuery();
  const { patch } = useListSearch();
  const boxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    registerSearch(boxRef.current?.querySelector('input') ?? null);
    return () => registerSearch(null);
  }, [registerSearch]);

  return (
    <Box
      ref={boxRef}
      component="search"
      data-testid="page-search"
    >
      <SimpleSearchBar
        value={query}
        onChange={(next) => patch({ query: next })}
        placeholder={searchPlaceholder}
        aria-label={searchPlaceholder}
        data-testid={`${testIdPrefix}-search-input`}
      />
    </Box>
  );
}

/** New, then the page's own buttons (Import…). */
function SidebarActions({ newAction, secondaryActions }: SidebarBodyProps): ReactNode {
  return (
    <Box sx={actionsSx}>
      {newAction !== undefined && (
        <BaseBtn
          variant="contained"
          startIcon={<AddOutlinedIcon />}
          onClick={newAction.onClick}
          disabled={newAction.disabled ?? false}
          data-testid={newAction.testId}
        >
          {newAction.label}
        </BaseBtn>
      )}
      {secondaryActions}
    </Box>
  );
}

/** The facets, the sort and the card/table switch. */
function SidebarFilters({ testIdPrefix, pageKey, facets = [], sortFields, viewToggle = true }: SidebarBodyProps): ReactNode {
  const { search, patch } = useListSearch();
  return (
    <>
      {facets.map((facet) => (
        <FacetGroup
          key={facet.param}
          facet={facet}
          search={search}
          patch={patch}
          testIdPrefix={testIdPrefix}
        />
      ))}
      {sortFields !== undefined && (
        <SortControl
          fields={sortFields}
          search={search}
          patch={patch}
          testIdPrefix={testIdPrefix}
        />
      )}
      {viewToggle && (
        <Section label={t('widgets.listSidebar.view', 'View')}>
          <ListViewToggle
            pageKey={pageKey}
            testIdPrefix={testIdPrefix}
          />
        </Section>
      )}
    </>
  );
}

const bodySx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  minHeight: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  padding: theme.spacing(2),
  overflowY: 'auto',
});

const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: 1 };
