/**
 * Try and Use in a chat for the skills of the project (ADR-0051 amendment 1, A1.3, A1.4), shared by the skills list's
 * card and row menu. A project skill is pinned by id, so nothing is forked. `dialog` is the "Use in a chat" picker
 * the page renders once.
 */
import { useState } from 'react';
import type { ReactNode } from 'react';

import { SkillToChatDialog, type SkillRecord } from '@/features/skills';
import { t } from '@/shared/i18n';
import { useTrySkill } from '@/pages/marketplace/useTrySkill';

export interface SkillChatActions {
  readonly onTry: ((skill: SkillRecord) => void) | undefined;
  readonly onUseInChat: (skill: SkillRecord) => void;
  readonly dialog: ReactNode;
}

function skillIdOf(skill: SkillRecord): number | undefined {
  const id = Number(skill.id);
  return Number.isInteger(id) && id > 0 ? id : undefined;
}

export function useSkillChatActions(projectId: string | undefined): SkillChatActions {
  const trySkill = useTrySkill();
  const [using, setUsing] = useState<SkillRecord | undefined>();
  const start = trySkill.start;
  return {
    onTry: start === undefined ? undefined : (skill) => {
      const id = skillIdOf(skill);
      if (id !== undefined) start({ name: skill.name, pin: { skill_id: id } });
    },
    onUseInChat: setUsing,
    dialog: (
      <SkillToChatDialog
        open={using !== undefined}
        projectId={projectId}
        skillName={using?.name ?? ''}
        title={t('skills.use.title', 'Use in a chat')}
        confirmText={t('skills.use.confirm', 'Use')}
        resolvePin={() => Promise.resolve({ skill_id: using === undefined ? 0 : (skillIdOf(using) ?? 0) })}
        onNewChat={(pin) => start?.({ name: using?.name ?? '', pin, kind: 'use' })}
        onClose={() => setUsing(undefined)}
      />
    ),
  };
}
