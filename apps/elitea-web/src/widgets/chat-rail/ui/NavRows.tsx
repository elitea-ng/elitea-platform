/**
 * The sidebar's place rows (web and desktop): one row per page (Applications,
 * Artifacts, …) and the "Workshop" — one row that opens to the things a
 * person builds with in the selected project: agents, pipelines, skills,
 * toolkits and MCPs. Each is a full page of its own, so the group only
 * shortens the list.
 *
 * A row that can create shows a "+" (New agent, …) at its end, on hover and
 * focus; it goes to the create page.
 *
 * The Workshop is closed until opened, and remembered (`useWorkshopOpen`).
 * Arriving on one of its pages (the palette, a link) opens it, so the page's
 * row is on screen.
 */
import { useEffect, type ReactNode } from 'react';

import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import AppsOutlinedIcon from '@mui/icons-material/AppsOutlined';
import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ChatBubbleOutlineOutlinedIcon from '@mui/icons-material/ChatBubbleOutlineOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HandymanOutlinedIcon from '@mui/icons-material/HandymanOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import Inventory2OutlinedIcon from '@mui/icons-material/Inventory2Outlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useWorkshopOpen } from '../model/useSectionOpen';
import { CHILD_INDENT, ShellRow, type RowCreate } from './rows';

/** One glyph per place, the same on the web and the desktop. */
export const NAV_ICONS: Readonly<Record<string, ReactNode>> = {
  chat: <ChatBubbleOutlineOutlinedIcon />,
  agents: <SmartToyOutlinedIcon />,
  pipelines: <AccountTreeOutlinedIcon />,
  skills: <AutoAwesomeOutlinedIcon />,
  toolkits: <BuildOutlinedIcon />,
  mcps: <HubOutlinedIcon />,
  applications: <AppsOutlinedIcon />,
  artifacts: <Inventory2OutlinedIcon />,
  credentials: <KeyOutlinedIcon />,
};

/** The pages the Workshop group holds. */
const WORKSHOP_ITEMS: ReadonlySet<string> = new Set(['agents', 'pipelines', 'skills', 'toolkits', 'mcps']);

interface NavRowItem {
  readonly value: string;
  readonly label: string;
  readonly url: string;
  /** The row's "+", when the person may create one here. */
  readonly create?: RowCreate | undefined;
}

interface NavRowProps {
  readonly item: NavRowItem;
  readonly selected: boolean;
  readonly indent?: number;
  /** Opens the page; without it the row is a link to `item.url`. */
  readonly go?: ((to: string) => void) | undefined;
}

function NavRow({ item, selected, indent = 0, go }: NavRowProps): React.JSX.Element {
  return (
    <ShellRow
      testId={`shell-elitea-${item.value}`}
      icon={NAV_ICONS[item.value]}
      label={item.label}
      indent={indent}
      selected={selected}
      {...(go === undefined ? { to: item.url } : { onClick: () => go(item.url) })}
      create={item.create}
    />
  );
}

interface WorkshopGroupProps {
  /** The Workshop items this person can open, in the sidebar's order. */
  readonly items: readonly NavRowItem[];
  readonly selected: string | undefined;
  readonly go?: ((to: string) => void) | undefined;
}

function WorkshopGroup({ items, selected, go }: WorkshopGroupProps): React.JSX.Element | null {
  const [open, toggle, expand] = useWorkshopOpen();
  const holdsSelected = selected !== undefined && items.some((item) => item.value === selected);
  useEffect(() => {
    if (holdsSelected) expand();
  }, [holdsSelected, expand]);
  if (items.length === 0) return null;
  return (
    <li>
      <ShellRow
        testId="shell-elitea-workshop"
        icon={<HandymanOutlinedIcon />}
        label={t('desktop.shell.workshop', 'Workshop')}
        ariaExpanded={open}
        // Closed over the page on screen: the group stands for it.
        selected={!open && holdsSelected}
        onClick={toggle}
        trailing={
          <Box aria-hidden sx={(theme: Theme) => ({ display: 'flex', color: theme.vars.palette.text.metrics, '& svg': { width: '0.875rem', height: '0.875rem' } })}>
            {open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
          </Box>
        }
      />
      {open && (
        <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
          {items.map((item) => (
            <li key={item.value}>
              <NavRow item={item} indent={CHILD_INDENT} selected={selected === item.value} go={go} />
            </li>
          ))}
        </Box>
      )}
    </li>
  );
}

export interface NavRowsProps {
  /** The places this person can open, in the sidebar's order; the Workshop's go into its group. */
  readonly items: readonly NavRowItem[];
  readonly selected: string | undefined;
  readonly go?: ((to: string) => void) | undefined;
}

/** The place rows, then the Workshop group. */
export function NavRows({ items, selected, go }: NavRowsProps): React.JSX.Element {
  return (
    <>
      {items
        .filter((item) => !WORKSHOP_ITEMS.has(item.value))
        .map((item) => (
          <li key={item.value}>
            <NavRow item={item} selected={selected === item.value} go={go} />
          </li>
        ))}
      <WorkshopGroup items={items.filter((item) => WORKSHOP_ITEMS.has(item.value))} selected={selected} go={go} />
    </>
  );
}
