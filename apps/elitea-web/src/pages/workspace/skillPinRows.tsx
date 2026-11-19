/**
 * What a project skill's row in the composer's "/" menu says about the thread's pins (#2183): a "Pinned" badge, and
 * "Pin to this thread" or "Unpin" beside it. A skill of the agent version is chosen by name per message and is not
 * pinned here.
 */
import { useMemo, useState } from 'react';

import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { describeWorkspaceError, type SuggestionItem } from '@/features/workspace';
import { t } from '@/shared/i18n';

import type { ComposerSkill } from './useAgentSkills';
import type { SkillPinControls } from './useThreadSkillPins';

export interface SkillPinRows {
  readonly pinned: ReadonlySet<string>;
  /** Pinned skills the person can no longer read: rows of their own, so they can be unpinned. */
  readonly stale: readonly string[];
  /** Pin or unpin `skill`; a refusal is shown by the caller. */
  readonly run: (act: 'pin' | 'unpin', skill: ComposerSkill) => void;
}

/** One row per stale pin, after the skills: the skill is gone, so only its id is known. */
export function staleSkillItems(rows: SkillPinRows): SuggestionItem[] {
  return rows.stale.map((id) => ({
    key: `stale-pin:${id}`,
    label: t('workspace.composer.skillGone', 'Skill {{id}} is not available', { id }),
    section: t('workspace.composer.skillsGone', 'Pinned skills you can no longer use'),
    action: { label: t('workspace.composer.skillUnpin', 'Unpin'), onClick: () => rows.run('unpin', { id, name: id }) },
  }));
}

export function skillPinExtras(skill: ComposerSkill, rows: SkillPinRows): Pick<SuggestionItem, 'badges' | 'action'> {
  if (skill.project !== true) return {};
  const pinned = rows.pinned.has(skill.id);
  return {
    ...(pinned ? { badges: [{ label: t('workspace.composer.skillPinned', 'Pinned') }] } : {}),
    action: pinned
      ? { label: t('workspace.composer.skillUnpin', 'Unpin'), onClick: () => rows.run('unpin', skill) }
      : { label: t('workspace.composer.skillPin', 'Pin to this thread'), onClick: () => rows.run('pin', skill) },
  };
}

/** The menu's pin rows for the thread's controls, and what a refused pin or unpin says (`null` until one is). */
export function useSkillPinRows(pins: SkillPinControls | undefined): { rows: SkillPinRows | undefined; error: string | null } {
  const [error, setError] = useState<string | null>(null);
  const rows = useMemo<SkillPinRows | undefined>(
    () =>
      pins === undefined
        ? undefined
        : {
            pinned: pins.pinned,
            stale: pins.stale,
            run: (act, skill) => {
              setError(null);
              void pins[act](skill).catch((failure: unknown) => setError(describeWorkspaceError(failure)));
            },
          },
    [pins],
  );
  return { rows, error: error ?? pins?.readError ?? null };
}

/** The refusal of a pin, above the composer. */
export function SkillPinError({ message }: { readonly message: string | null }): React.JSX.Element | null {
  if (message === null) return null;
  return (
    <Typography variant="bodySmall" role="alert" data-testid="skill-pin-error" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
      {message}
    </Typography>
  );
}
