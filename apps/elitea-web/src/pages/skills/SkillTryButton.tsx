/**
 * **Test skill** in the skill editor (ADR-0051 amendment 1, A1.3): Try on the SAVED skill. A pin names the skill, and
 * the text is read when each turn starts, so unsaved edits would not be what runs. While the form holds unsaved
 * edits the button is off and says "Save to try".
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { useTrySkill } from '@/pages/marketplace/useTrySkill';

export interface SkillTryButtonProps {
  readonly skillId: string | undefined;
  readonly name: string;
  readonly isDirty: boolean;
}

export function SkillTryButton({ skillId, name, isDirty }: SkillTryButtonProps): ReactNode {
  const trySkill = useTrySkill();
  const id = Number(skillId);
  const known = Number.isInteger(id) && id > 0;
  return (
    <>
      <BaseBtn
        variant="secondary"
        disabled={isDirty || !known || trySkill.start === undefined || trySkill.isStarting}
        onClick={() => trySkill.start?.({ name, pin: { skill_id: id } })}
        data-testid="skill-test-button"
      >
        {isDirty ? t('skills.try.saveFirst', 'Save to try') : t('skills.try.test', 'Test skill')}
      </BaseBtn>
      {trySkill.isError && (
        <Typography role="alert" variant="bodySmall" color="error">
          {t('skills.try.failed', 'The chat could not be started. Try again.')}
        </Typography>
      )}
    </>
  );
}
