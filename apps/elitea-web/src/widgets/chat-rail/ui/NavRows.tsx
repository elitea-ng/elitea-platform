/**
 * The sidebar's "Workshop": one row that opens to the things a person builds
 * with in the selected project — agents, pipelines, skills, toolkits and MCPs
 * (`WORKSHOP_ITEMS`). Each is a full page of its own, so the group only
 * shortens the list; it stays in the Elitea section, under that project.
 *
 * Closed until opened, and remembered (`useWorkshopOpen`). Arriving on one of
 * its pages (the palette, a link) opens it, so the page's row is on screen.
 */
import { useEffect } from 'react';

import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HandymanOutlinedIcon from '@mui/icons-material/HandymanOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { EliteaItem, EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { useWorkshopOpen } from '../model/useEliteaSectionOpen';
import { CHILD_INDENT, ShellRow } from './rows';

export const WORKSHOP_ICONS: Partial<Record<EliteaItemValue, React.JSX.Element>> = {
  agents: <SmartToyOutlinedIcon />,
  pipelines: <AccountTreeOutlinedIcon />,
  skills: <AutoAwesomeOutlinedIcon />,
  toolkits: <BuildOutlinedIcon />,
  mcps: <HubOutlinedIcon />,
};

export interface WorkshopGroupProps {
  /** The Workshop items this person can open, in the sidebar's order. */
  readonly items: readonly EliteaItem[];
  readonly selected: EliteaItemValue | undefined;
  readonly run: (action: ShellAction) => void;
}

export function WorkshopGroup({ items, selected, run }: WorkshopGroupProps): React.JSX.Element | null {
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
              <ShellRow
                testId={`shell-elitea-${item.value}`}
                icon={WORKSHOP_ICONS[item.value]}
                label={item.label}
                indent={CHILD_INDENT}
                selected={selected === item.value}
                onClick={() => run({ type: 'go', to: item.url })}
              />
            </li>
          ))}
        </Box>
      )}
    </li>
  );
}
