import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { validateSkill } from '../lib/skillValidation';
import type { SkillDraft } from '../model/types';
import { SkillForm } from './SkillForm';

export interface GenerateSkillModalProps {
  readonly open: boolean;
  readonly isGenerating: boolean;
  readonly onClose: () => void;
  readonly onGenerate: (description: string) => Promise<SkillDraft>;
  readonly onApprove: (draft: SkillDraft) => void;
}

export function GenerateSkillModal({
  open,
  isGenerating,
  onClose,
  onGenerate,
  onApprove,
}: GenerateSkillModalProps): ReactNode {
  const [description, setDescription] = useState('');
  const [draft, setDraft] = useState<SkillDraft>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (!open) {
      setDescription('');
      setDraft(undefined);
      setError(undefined);
    }
  }, [open]);

  const handleConfirm = (): void => {
    if (draft) {
      if (Object.keys(validateSkill(draft)).length === 0) onApprove(draft);
      return;
    }
    if (!description.trim()) {
      setError(t('skills.generate.descriptionRequired', 'Describe the skill you want to create.'));
      return;
    }
    void onGenerate(description).then(setDraft).catch(() => {
      setError(t('skills.generate.error', 'Failed to generate a skill draft.'));
    });
  };

  return (
    <BaseModal
      open={open}
      variant="complex"
      title={t('skills.generate.title', 'Generate skill with AI')}
      onClose={onClose}
      onConfirm={handleConfirm}
      actions={{
        confirmText: draft
          ? t('skills.generate.useDraft', 'Use draft')
          : t('skills.generate.generate', 'Generate'),
        confirming: isGenerating,
      }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          {draft ? (
            <SkillForm
              value={draft}
              onChange={setDraft}
              showErrors
            />
          ) : (
            <TextField
              multiline
              minRows={8}
              maxRows={16}
              fullWidth
              // A placeholder in the shared placeholder colour, not a floating
              // label: the label rendered cyan on the dark surface and was
              // nearly invisible (#2099). Same shape as the agent generator.
              placeholder={t(
                'skills.generate.placeholder',
                'Describe what this skill should do, when it applies and any rules it must follow.',
              )}
              value={description}
              onChange={(event) => setDescription(event.target.value)}
              variant="standard"
              slotProps={{
                input: { disableUnderline: true },
                htmlInput: { 'aria-label': t('skills.generate.description', 'What should this skill do?') },
              }}
            />
          )}
          {error && <Typography role="alert">{error}</Typography>}
        </Box>
      }
    />
  );
}
