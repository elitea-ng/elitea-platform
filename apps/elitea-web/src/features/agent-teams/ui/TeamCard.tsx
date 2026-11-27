/**
 * One saved team on the Teams page (issue 3134), on the same surface as the
 * Agents cards: its name and a two-line description, the members as an
 * avatar stack (lead first, with its badge), a muted line with when it last
 * changed, a primary Start conversation, and Export, Edit and Delete in an
 * overflow menu. Delete is destructive and asks first (the page's
 * confirmation dialog).
 */
import { useState, type MouseEvent, type ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import StorefrontOutlinedIcon from '@mui/icons-material/StorefrontOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentTeam } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { cardSurfaceSx } from '@/shared/lib/cardSurface';
import { updatedAgoLabel } from '@/shared/lib/updatedAgo';

import type { MemberAvatar } from '../model/useTeamMemberAvatars';
import { TeamAvatarStack, type RenderMemberAvatar } from './TeamAvatarStack';

export interface TeamCardProps {
  readonly team: AgentTeam;
  readonly avatars: ReadonlyMap<string, MemberAvatar>;
  readonly renderAvatar?: RenderMemberAvatar | undefined;
  readonly starting: boolean;
  readonly onStart: () => void;
  /** Saves the team as a zip that Workshop › Agents › Import reads back (issue 3010). */
  readonly onExport: () => void;
  readonly exporting: boolean;
  /** Lists the team in the Marketplace (issue 3137); absent for a caller who may not publish. */
  readonly onPublish?: (() => void) | undefined;
  readonly publishing?: boolean | undefined;
  /** Absent for a caller who may not edit teams. */
  readonly onEdit?: (() => void) | undefined;
  /** Absent for a caller who may not delete teams; opens the confirmation. */
  readonly onDelete?: (() => void) | undefined;
}

export function TeamCard({ team, avatars, renderAvatar, starting, onStart, onExport, exporting, onPublish, publishing, onEdit, onDelete }: TeamCardProps): ReactNode {
  const updated = updatedAgoLabel(team.updated_at);
  return (
    <Box
      component="article"
      aria-labelledby={`agent-team-name-${String(team.id)}`}
      data-testid={`agent-team-${String(team.id)}`}
      sx={cardSx}
    >
      <Box sx={headerSx}>
        <Typography
          component="h2"
          variant="headingSmall"
          id={`agent-team-name-${String(team.id)}`}
          data-testid={`agent-team-name-${String(team.id)}`}
          sx={nameSx}
        >
          {team.name}
        </Typography>
        <TeamMenu
          team={team}
          exporting={exporting}
          onExport={onExport}
          onPublish={onPublish}
          publishing={publishing}
          onEdit={onEdit}
          onDelete={onDelete}
        />
      </Box>
      {team.description !== '' && (
        <Typography
          variant="bodyMedium"
          data-testid={`agent-team-description-${String(team.id)}`}
          sx={descriptionSx}
        >
          {team.description}
        </Typography>
      )}
      <TeamAvatarStack
        team={team}
        avatars={avatars}
        renderAvatar={renderAvatar}
      />
      <Box sx={footerSx}>
        {updated !== undefined && (
          <Box
            component="time"
            dateTime={team.updated_at}
            sx={updatedSx}
          >
            {updated}
          </Box>
        )}
        <Button
          size="small"
          variant="contained"
          disabled={starting}
          onClick={onStart}
          data-testid={`agent-team-start-${String(team.id)}`}
          sx={startSx}
        >
          {t('features.agentTeams.start', 'Start conversation')}
        </Button>
      </Box>
    </Box>
  );
}

/** The overflow (⋯) menu: Export, Publish to Marketplace, Edit, then Delete after a divider in the error colour. */
function TeamMenu({
  team,
  exporting,
  onExport,
  onPublish,
  publishing = false,
  onEdit,
  onDelete,
}: Pick<TeamCardProps, 'team' | 'exporting' | 'onExport' | 'onPublish' | 'publishing' | 'onEdit' | 'onDelete'>): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const label = t('features.agentTeams.moreActions', 'More actions for {{name}}', { name: team.name });
  const run = (action: () => void) => () => {
    setAnchor(null);
    action();
  };
  return (
    <>
      <IconButton
        size="small"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        data-testid={`agent-team-menu-${String(team.id)}`}
        onClick={(event: MouseEvent<HTMLElement>) => setAnchor(event.currentTarget)}
      >
        <MoreHorizIcon fontSize="small" />
      </IconButton>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => setAnchor(null)}
        slotProps={{ list: { 'aria-label': label } }}
      >
        <MenuItem
          disabled={exporting}
          onClick={run(onExport)}
          data-testid={`agent-team-export-${String(team.id)}`}
        >
          <ListItemIcon>
            <FileDownloadOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('features.agentTeams.exportAction', 'Export')}</ListItemText>
        </MenuItem>
        {onPublish !== undefined && (
          <MenuItem
            disabled={publishing}
            onClick={run(onPublish)}
            data-testid={`agent-team-publish-${String(team.id)}`}
          >
            <ListItemIcon>
              <StorefrontOutlinedIcon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{t('features.agentTeams.publishAction', 'Publish to Marketplace')}</ListItemText>
          </MenuItem>
        )}
        {onEdit !== undefined && (
          <MenuItem
            onClick={run(onEdit)}
            data-testid={`agent-team-edit-${String(team.id)}`}
          >
            <ListItemIcon>
              <EditOutlinedIcon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{t('features.agentTeams.editAction', 'Edit')}</ListItemText>
          </MenuItem>
        )}
        {onDelete !== undefined && <Divider />}
        {onDelete !== undefined && (
          <MenuItem
            onClick={run(onDelete)}
            data-testid={`agent-team-delete-${String(team.id)}`}
            sx={dangerSx}
          >
            <ListItemIcon sx={inheritColorSx}>
              <DeleteOutlineIcon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{t('features.agentTeams.deleteAction', 'Delete')}</ListItemText>
          </MenuItem>
        )}
      </Menu>
    </>
  );
}

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  ...cardSurfaceSx(theme),
  height: '100%',
  minHeight: '11rem',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1.5),
  padding: theme.spacing(2),
});

const headerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'flex-start', gap: theme.spacing(1), minWidth: 0 });

const nameSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  minWidth: 0,
  margin: 0,
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  alignSelf: 'center',
});

const descriptionSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  wordBreak: 'break-word',
  overflow: 'hidden',
  display: '-webkit-box',
  WebkitBoxOrient: 'vertical',
  WebkitLineClamp: '2',
});

const footerSx: SxProps<Theme> = (theme: Theme) => ({ marginTop: 'auto', display: 'flex', alignItems: 'center', gap: theme.spacing(1), minWidth: 0 });

const updatedSx: SxProps<Theme> = (theme: Theme) => ({ ...theme.typography.bodySmall, color: theme.vars.palette.text.primary, whiteSpace: 'nowrap' });

const startSx: SxProps<Theme> = { marginLeft: 'auto', flexShrink: 0 };

const dangerSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.error.main });

const inheritColorSx: SxProps<Theme> = { color: 'inherit' };
