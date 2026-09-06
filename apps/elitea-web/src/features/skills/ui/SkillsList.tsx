import { useMemo, type ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import DownloadOutlinedIcon from '@mui/icons-material/DownloadOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { EntityCardList, EntityEmptyState, entityTypeIcon, type EntityListItem } from '@/shared/ui/EntityCardList';

import type { SkillRecord } from '../model/types';

/**
 * The skills list — a grid of `shared/ui/EntityCardList` cards (icon tile,
 * title, author/tag bottom row), the same surface every other entity list
 * page renders, with the per-skill export/delete affordances in the card's
 * top-right actions slot.
 *
 * This used to be a bordered MUI `<List>` of plain rows, from the era before
 * a card grid was ported. The zero-state is now the baseline's real
 * `EmptyStatePage` (`[fsd]/entities/empty-state-page`): the skills
 * illustration, "No skills yet", the two-line explanation, and a `+ Create`
 * pill — matching the production reference exactly.
 */
export interface SkillsListProps {
  readonly items: readonly SkillRecord[];
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly query: string;
  readonly onSelect: (skillId: string) => void;
  readonly onDelete: (skill: SkillRecord) => void;
  readonly onExport: (skill: SkillRecord) => void;
  /** Wired to the empty state's `+ Create` CTA. */
  readonly onCreate?: () => void;
  /** `false` when the owning page already reserved the rail's width. */
  readonly railVisible?: boolean;
}

export function SkillsList({ items, isLoading, isError, query, onSelect, onDelete, onExport, onCreate, railVisible }: SkillsListProps): ReactNode {
  const listItems = useMemo<EntityListItem[]>(
    () =>
      items.map((skill) => ({
        id: skill.id,
        name: skill.name || t('skills.list.untitled', 'Untitled skill'),
        description: skill.description ?? '',
        icon: entityTypeIcon('skill'),
        tags: (skill.tags ?? []).map((tag) => ({ id: tag, name: tag })),
        createdAt: skill.created_at,
        onClick: () => {
          onSelect(skill.id);
        },
      })),
    [items, onSelect],
  );

  const bySkillId = useMemo(() => new Map(items.map((skill) => [skill.id, skill])), [items]);

  return (
    <EntityCardList
      items={listItems}
      isLoading={isLoading}
      isError={isError}
      errorMessage={t('skills.list.error', 'Failed to load skills.')}
      {...(railVisible === undefined ? {} : { railVisible })}
      emptyState={
        <EntityEmptyState
          art="skills"
          title={query.trim() ? t('skills.list.noMatches', 'No matching skills.') : t('skills.list.empty', 'No skills yet')}
          description={t(
            'skills.list.emptyDescription',
            'Create your first skill to get started. Skills are reusable, markdown-based instructions you can attach to your agents.',
          )}
          {...(onCreate === undefined ? {} : { onCreateClick: onCreate })}
        />
      }
      renderCardActions={(item) => {
        const skill = bySkillId.get(item.id);
        if (skill === undefined) return null;
        return (
          <SkillCardActions
            skill={skill}
            onDelete={onDelete}
            onExport={onExport}
          />
        );
      }}
    />
  );
}

interface SkillCardActionsProps {
  readonly skill: SkillRecord;
  readonly onDelete: (skill: SkillRecord) => void;
  readonly onExport: (skill: SkillRecord) => void;
}

/** `Card.jsx`'s `topRightSection` slot — the per-row actions the old `<List>` rendered as a `secondaryAction`. */
function SkillCardActions({ skill, onDelete, onExport }: SkillCardActionsProps): ReactNode {
  return (
    <>
      <Tooltip title={t('skills.list.export', 'Export')}>
        <IconButton
          size="small"
          aria-label={t('skills.list.export', 'Export')}
          onClick={(event) => {
            event.stopPropagation();
            onExport(skill);
          }}
        >
          <DownloadOutlinedIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Tooltip title={t('skills.list.delete', 'Delete')}>
        <IconButton
          size="small"
          aria-label={t('skills.list.delete', 'Delete')}
          onClick={(event) => {
            event.stopPropagation();
            onDelete(skill);
          }}
        >
          <DeleteOutlineIcon fontSize="small" />
        </IconButton>
      </Tooltip>
    </>
  );
}
