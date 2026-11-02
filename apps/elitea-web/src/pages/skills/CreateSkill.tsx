import type { ReactNode } from 'react';
import { useState } from 'react';

import SaveOutlinedIcon from '@mui/icons-material/SaveOutlined';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import {
  GenerateSkillModal,
  isSkillValid,
  SkillForm,
  useSkillMutations,
  type SkillDraft,
  type SkillWriteInput,
} from '@/features/skills';
import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { PageToolbar, pageSx } from '@/widgets/page-header';

import { useSelectedProjectId } from './lib/useSelectedProjectId';

const EMPTY_SKILL: SkillWriteInput = { name: '', description: '', instructions: '', tags: [] };

export function CreateSkill(): ReactNode {
  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const mutations = useSkillMutations(projectId);
  const [value, setValue] = useState<SkillWriteInput>(EMPTY_SKILL);
  const [showErrors, setShowErrors] = useState(false);
  const [generateOpen, setGenerateOpen] = useState(false);
  const [error, setError] = useState<string>();
  usePageCrumbs([{ label: t('skills.create.title', 'New Skill') }]);

  // `SkillForm` renders the Generate button only when it receives
  // `onGenerate`. The draft endpoint is not mounted, so it receives nothing —
  // see `shared/config/backendCapabilities`.
  const generateProps = hasBackendCapability('aiGeneration') ? { onGenerate: () => setGenerateOpen(true) } : {};

  const save = (): void => {
    setShowErrors(true);
    if (!isSkillValid(value) || !projectId) return;
    setError(undefined);
    void mutations.create
      .mutateAsync(value)
      .then((skill) =>
        navigate({
          to: '/skills/$tab/$skillId',
          params: { tab: 'all', skillId: skill.id },
          replace: true,
        }),
      )
      .catch(() => setError(t('skills.create.error', 'Failed to create the skill.')));
  };

  return (
    <Box sx={pageSx}>
      <PageToolbar
        secondaryActions={
          <>
            <BaseBtn
              variant="secondary"
              disabled={mutations.create.isPending}
              onClick={() => void navigate({ to: '/skills/$tab', params: { tab: 'all' } })}
            >
              {t('common.cancel', 'Cancel')}
            </BaseBtn>
            <BaseBtn
              variant="contained"
              startIcon={<SaveOutlinedIcon />}
              disabled={value === EMPTY_SKILL || mutations.create.isPending}
              onClick={save}
            >
              {t('skills.toolbar.save', 'Save')}
            </BaseBtn>
          </>
        }
      />
      <Box sx={contentSx}>
        {error && <Typography role="alert">{error}</Typography>}
        <SkillForm
          value={value}
          onChange={setValue}
          disabled={mutations.create.isPending}
          showErrors={showErrors}
          {...generateProps}
        />
      </Box>
      <GenerateSkillModal
        open={generateOpen}
        isGenerating={mutations.generate.isPending}
        onClose={() => setGenerateOpen(false)}
        onGenerate={(description) => mutations.generate.mutateAsync(description)}
        onApprove={(draft: SkillDraft) => {
          setValue(draft);
          setGenerateOpen(false);
        }}
      />
    </Box>
  );
}

const contentSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  overflowY: 'auto',
  padding: theme.spacing(3),
});
