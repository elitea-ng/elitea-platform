import { useState, type ReactNode } from 'react';

import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { BrandLogoMark } from '@/shared/ui/brand-logo';
import { t } from '@/shared/i18n';

import { NotificationButton } from './NotificationButton';
import { SidebarConnectionDot } from './SidebarConnectionDot';

export interface SidebarHeaderProps {
  collapsed: boolean;
  onToggleCollapsed: () => void;
}

/**
 * The rail's top row: the logo (it toggles the rail too), the notifications
 * bell and the collapse toggle — an ordinary button in the row, not a circle
 * pinned over the rail's edge. Collapsed, the row stacks the logo over the
 * toggle; the bell waits for the rail to open again.
 *
 * The logo is `BrandLogoMark` (ADR-0024 WP3): a served pack's own
 * `assets.logoMark`, else the compiled orb. `SidebarConnectionDot` sits on it
 * and renders nothing until a live SSE channel reports its health.
 */
export function SidebarHeader({ collapsed, onToggleCollapsed }: SidebarHeaderProps): ReactNode {
  const toggleLabel = collapsed ? t('widgets.sidebar.expand', 'Expand sidebar') : t('widgets.sidebar.collapse', 'Collapse sidebar');
  // Controlled, and closed by the click: the toggle moves when the rail
  // changes width, out from under the pointer, so no `mouseleave` ever came
  // and its tooltip stayed on screen over the collapsed rail.
  const [tipOpen, setTipOpen] = useState(false);
  const toggle = (): void => {
    setTipOpen(false);
    onToggleCollapsed();
  };
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: collapsed ? 'column' : 'row',
        alignItems: 'center',
        gap: collapsed ? 0 : 0.5,
        padding: collapsed ? '0.5rem 0' : '0 0.5rem 0 1rem',
        width: '100%',
        boxSizing: 'border-box',
        minHeight: '3.75rem',
      }}
    >
      <IconButton
        data-testid="sidebar-toggle"
        size="large"
        color="inherit"
        aria-label={t('widgets.sidebar.toggle', 'Toggle sidebar')}
        onClick={onToggleCollapsed}
        sx={{ position: 'relative', width: '2.75rem', height: '2.75rem', paddingInline: 0, marginLeft: collapsed ? 0 : '-0.5rem' }}
      >
        <BrandLogoMark style={{ width: '1.75rem', height: '1.75rem' }} />
        <SidebarConnectionDot />
      </IconButton>
      {!collapsed && <Box sx={{ flex: 1 }} />}
      {/* Collapsed too: the bell is the only way to the notifications, so the narrow rail keeps it as an icon. */}
      <NotificationButton />
      <Tooltip title={toggleLabel} placement={collapsed ? 'right' : 'bottom'} open={tipOpen} onOpen={() => setTipOpen(true)} onClose={() => setTipOpen(false)}>
        <IconButton size="small" data-testid="sidebar-collapse-toggle" aria-label={toggleLabel} aria-expanded={!collapsed} onClick={toggle}>
          {collapsed ? <KeyboardDoubleArrowRightIcon fontSize="small" /> : <KeyboardDoubleArrowLeftIcon fontSize="small" />}
        </IconButton>
      </Tooltip>
    </Box>
  );
}
