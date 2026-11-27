/**
 * One blueprint of the gallery (issue 3137), on the Agents and Teams cards'
 * surface (`cardSurfaceSx`): its avatar (a team shows its members, lead
 * first), name, description, what it showcases, what it needs connected,
 * where it runs, and the two actions — Import, and Craft from it (Build with
 * AI with the blueprint's brief).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { AgentAvatar } from '@/features/chat-messages';
import { t } from '@/shared/i18n';
import { cardSurfaceSx } from '@/shared/lib/cardSurface';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { blueprintPath, type BlueprintAvatar, type BlueprintEntry } from '../model/blueprintIndex';

export interface BlueprintCardProps {
  readonly entry: BlueprintEntry;
  readonly importing: boolean;
  readonly onImport: (entry: BlueprintEntry) => void;
  /** Absent when the deployment does not serve Build with AI. */
  readonly onCraft?: ((entry: BlueprintEntry) => void) | undefined;
}

const MAX_FACES = 4;

function Face({ avatar, size }: { readonly avatar: BlueprintAvatar; readonly size: string }): ReactNode {
  return (
    <AgentAvatar
      iconUrl={avatar.still === undefined ? undefined : `${import.meta.env.BASE_URL}${blueprintPath(avatar.still)}`}
      avatarKey={avatar.defaultName === undefined ? undefined : `default:${avatar.defaultName}`}
      size={size}
    />
  );
}

function Faces({ entry }: { readonly entry: BlueprintEntry }): ReactNode {
  if (entry.kind === 'agent') return <Face avatar={entry.avatar} size="2.5rem" />;
  return (
    <Box sx={facesSx}>
      {entry.members.slice(0, MAX_FACES).map((member) => (
        <Face key={member.slug} avatar={member.avatar} size="2rem" />
      ))}
    </Box>
  );
}

function surfaceLabel(entry: BlueprintEntry): string {
  return entry.surface === 'desktop'
    ? t('widgets.blueprintGallery.surfaceDesktop', 'Desktop · Local work')
    : t('widgets.blueprintGallery.surfaceCloud', 'Cloud');
}

function Facts({ entry }: { readonly entry: BlueprintEntry }): ReactNode {
  return (
    <>
      {entry.showcases.length > 0 && (
        <Box sx={rowSx}>
          <Typography variant="labelSmall" sx={labelSx}>
            {t('widgets.blueprintGallery.showcases', 'Showcases')}
          </Typography>
          {entry.showcases.map((item) => (
            <Chip key={item} size="small" variant="outlined" label={item} />
          ))}
        </Box>
      )}
      {entry.requirements.length > 0 && (
        <Box>
          <Typography variant="labelSmall" sx={labelSx}>
            {t('widgets.blueprintGallery.needs', 'Needs')}
          </Typography>
          {entry.requirements.map((item) => (
            <Typography key={`${item.type}-${item.label}`} variant="bodySmall">
              {item.label}
            </Typography>
          ))}
        </Box>
      )}
    </>
  );
}

export function BlueprintCard({ entry, importing, onImport, onCraft }: BlueprintCardProps): ReactNode {
  return (
    <Box component="article" sx={cardSx} data-testid={`blueprint-card-${entry.slug}`} aria-label={entry.name}>
      <Box sx={headerSx}>
        <Faces entry={entry} />
        <Typography component="h3" variant="headingSmall" sx={nameSx}>
          {entry.name}
        </Typography>
        <Chip size="small" label={surfaceLabel(entry)} />
      </Box>
      <Typography variant="bodyMedium" sx={descriptionSx}>
        {entry.description}
      </Typography>
      <Facts entry={entry} />
      <Box sx={actionsSx}>
        {onCraft !== undefined && entry.brief !== '' && (
          <BaseBtn variant="tertiary" size="small" data-testid={`blueprint-craft-${entry.slug}`} onClick={() => onCraft(entry)}>
            {t('widgets.blueprintGallery.craft', 'Craft from it')}
          </BaseBtn>
        )}
        <BaseBtn variant="secondary" size="small" disabled={importing} data-testid={`blueprint-import-${entry.slug}`} onClick={() => onImport(entry)}>
          {t('widgets.blueprintGallery.import', 'Import')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  ...cardSurfaceSx(theme),
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1.5),
  padding: theme.spacing(2),
});
const headerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1), minWidth: 0 });
const facesSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', gap: theme.spacing(0.5), flexShrink: 0 });
const nameSx: SxProps<Theme> = { flex: 1, minWidth: 0, margin: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const descriptionSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  wordBreak: 'break-word',
  overflow: 'hidden',
  display: '-webkit-box',
  WebkitBoxOrient: 'vertical',
  WebkitLineClamp: '3',
});
const rowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: theme.spacing(0.5) });
const labelSx: SxProps<Theme> = (theme: Theme) => ({ display: 'block', color: theme.vars.palette.text.secondary, marginRight: theme.spacing(0.5) });
const actionsSx: SxProps<Theme> = (theme: Theme) => ({ marginTop: 'auto', display: 'flex', justifyContent: 'flex-end', gap: theme.spacing(1) });
