/**
 * The listing page's **Hire** split button (ADR-0051 decision 8): the main
 * button opens the hire wizard on Install, the arrow offers Install or Fork.
 * When this project already hired the listing the page also says "Hired
 * v2.3", and the button reads "Hire again" (a project may hire a listing more
 * than once, for example two versions side by side).
 */
import { useRef, useState, type ReactNode } from 'react';

import ArrowDropDownIcon from '@mui/icons-material/ArrowDropDown';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonGroup from '@mui/material/ButtonGroup';
import Chip from '@mui/material/Chip';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';

import type { MarketplaceListingDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePermissionSet } from '@/widgets/sidebar';

import { HireWizard } from '../hire/HireWizard';
import { TeamHireDialog } from '../hire/TeamHireDialog';
import type { HireMode } from '../hire/hireModel';

/** models.teams.create (shared 0233): a team hire saves a team. */
const TEAM_CREATE_PERMISSION = 'models.teams.create';

export interface HireButtonProps {
  readonly projectId: string | undefined;
  readonly detail: MarketplaceListingDetail;
}

export function HireButton({ projectId, detail }: HireButtonProps): ReactNode {
  const permissions = usePermissionSet(projectId);
  const anchor = useRef<HTMLDivElement>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const [mode, setMode] = useState<HireMode | undefined>();
  const hired = detail.listing.hired[0];
  const delisted = detail.listing.state === 'delisted';
  const { team } = detail;
  // A team hire creates agents AND a saved team, so it also needs the team create permission.
  const canHire =
    permissions.has(PERMISSIONS.applications.create) &&
    (team === undefined || permissions.has(TEAM_CREATE_PERMISSION)) &&
    !delisted &&
    projectId !== undefined;
  const pick = (next: HireMode): void => {
    setMenuOpen(false);
    setMode(next);
  };
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      {hired !== undefined && (
        <Chip
          size="small"
          color="primary"
          label={t('marketplace.listing.hired', 'Hired {{version}}', { version: hired.version_name })}
          data-testid="marketplace-hired"
        />
      )}
      <ButtonGroup ref={anchor} variant="contained" disabled={!canHire}>
        <Button onClick={() => pick('install')} data-testid="marketplace-hire">
          {hired === undefined
            ? t('marketplace.listing.hire', 'Hire')
            : t('marketplace.listing.hireAgain', 'Hire again')}
        </Button>
        <Button
          size="small"
          aria-label={t('marketplace.listing.hireModes', 'Choose how to hire')}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen(true)}
          data-testid="marketplace-hire-menu"
        >
          <ArrowDropDownIcon />
        </Button>
      </ButtonGroup>
      <Menu anchorEl={anchor.current} open={menuOpen} onClose={() => setMenuOpen(false)}>
        <MenuItem onClick={() => pick('install')} data-testid="marketplace-hire-install">
          {t('marketplace.listing.install', 'Install')}
        </MenuItem>
        <MenuItem onClick={() => pick('fork')} data-testid="marketplace-hire-fork">
          {t('marketplace.listing.fork', 'Fork')}
        </MenuItem>
      </Menu>
      {team === undefined ? (
        <HireWizard
          open={mode !== undefined}
          onClose={() => setMode(undefined)}
          projectId={projectId}
          detail={detail}
          initialMode={mode ?? 'install'}
        />
      ) : (
        <TeamHireDialog
          open={mode !== undefined}
          onClose={() => setMode(undefined)}
          projectId={projectId}
          detail={detail}
          team={team}
          initialMode={mode ?? 'install'}
        />
      )}
    </Box>
  );
}
