/**
 * The web shell's title row, over the page: the breadcrumb that names what is
 * open (`Toolkits ▸ New toolkit`, `Agents ▸ My agent`, or the section alone on
 * a list) and, on the right, the side panel's toggle. The desktop frame's own
 * title row (`widgets/desktop-shell`'s `PageTitle`) is its counterpart.
 *
 * The page's heading is the LAST crumb and the row's one `<h1>`: pages draw no
 * title or back arrow of their own (`shared/ui/page-chrome`). The section crumb
 * before it links back to the section's list.
 */
import type { ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Link from '@mui/material/Link';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { Link as RouterLink, useRouterState } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { useShellCrumbs, type PageCrumb } from '@/shared/ui/page-chrome';

import { pageSection } from '../lib/pageTitle';
import type { WebSidePanelHost } from '../model/useWebSidePanelHost';

/** The section crumb, then the page's own; only the last one is the heading and only earlier ones link. */
export function shellCrumbs(pathname: string, pageCrumbs: readonly PageCrumb[]): readonly PageCrumb[] {
  const section = pageSection(pathname);
  if (section === undefined) return pageCrumbs;
  return [{ label: section.label, to: section.to }, ...pageCrumbs];
}

function Separator(): ReactNode {
  return (
    <ChevronRightIcon
      aria-hidden
      sx={separatorSx}
    />
  );
}

function Crumbs({ crumbs }: { readonly crumbs: readonly PageCrumb[] }): ReactNode {
  return (
    <Box
      component="nav"
      aria-label={t('shell.topBar.breadcrumb', 'Breadcrumb')}
      sx={crumbsSx}
    >
      {crumbs.map((crumb, index) => {
        const last = index === crumbs.length - 1;
        return (
          <Box
            key={`${String(index)}:${crumb.label}`}
            sx={crumbSlotSx(last)}
          >
            {index > 0 && <Separator />}
            {last ? (
              <Typography
                component="h1"
                variant="labelMedium"
                data-testid="shell-title"
                sx={currentSx}
              >
                {crumb.label}
              </Typography>
            ) : crumb.to === undefined ? (
              <Typography
                component="span"
                variant="labelMedium"
                sx={ancestorSx}
              >
                {crumb.label}
              </Typography>
            ) : (
              <Link
                component={RouterLink}
                to={crumb.to}
                underline="hover"
                variant="labelMedium"
                data-testid="shell-crumb-link"
                sx={ancestorSx}
              >
                {crumb.label}
              </Link>
            )}
          </Box>
        );
      })}
    </Box>
  );
}

function SidePanelToggle({ host }: { readonly host: WebSidePanelHost }): ReactNode {
  if (host.tabs.length === 0) return null;
  const label = host.shown ? t('shell.topBar.hidePanel', 'Hide side panel') : t('shell.topBar.showPanel', 'Show side panel');
  return (
    <Tooltip title={label}>
      <IconButton
        size="small"
        aria-label={label}
        aria-pressed={host.shown}
        data-testid="shell-side-panel-toggle"
        onClick={() => host.setOpen(!host.shown)}
      >
        <ViewSidebarOutlinedIcon
          fontSize="inherit"
          sx={mirroredSx}
        />
      </IconButton>
    </Tooltip>
  );
}

export function ShellTopBar({ sidePanel }: { readonly sidePanel: WebSidePanelHost }): ReactNode {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const crumbs = shellCrumbs(pathname, useShellCrumbs());
  return (
    <Box
      data-testid="shell-top-bar"
      sx={barSx}
    >
      <Crumbs crumbs={crumbs} />
      <SidePanelToggle host={sidePanel} />
    </Box>
  );
}

const barSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  height: '2.75rem',
  boxSizing: 'border-box',
  display: 'flex',
  alignItems: 'center',
  gap: 1,
  paddingInline: theme.spacing(3, 1.5),
  borderBottom: `1px solid ${theme.vars.palette.divider}`,
});

const crumbsSx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', alignItems: 'center', gap: 0.75 };

const crumbSlotSx =
  (last: boolean): SxProps<Theme> =>
  () => ({ display: 'flex', alignItems: 'center', gap: 0.75, minWidth: last ? 0 : undefined, flexShrink: last ? 1 : 0 });

const oneLine = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

const currentSx: SxProps<Theme> = (theme: Theme) => ({ ...oneLine, margin: 0, color: theme.vars.palette.text.secondary });

const ancestorSx: SxProps<Theme> = (theme: Theme) => ({ ...oneLine, color: theme.vars.palette.text.metrics });

const separatorSx: SxProps<Theme> = (theme: Theme) => ({ fontSize: theme.typography.labelMedium.fontSize, color: theme.vars.palette.text.metrics, flexShrink: 0, marginX: -0.25 });

/** The icon draws its bar on the left; the panel is on the right. */
const mirroredSx: SxProps<Theme> = { transform: 'scaleX(-1)' };
