import type { ChangeEvent, ReactNode } from 'react';
import { useMemo, useState } from 'react';

import AutoAwesomeIcon from '@mui/icons-material/AutoAwesome';
import ImageOutlinedIcon from '@mui/icons-material/ImageOutlined';
import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { CharacterCounter } from '@/shared/ui/CharacterCounter';
import { Markdown } from '@/shared/ui/Markdown';

import type { SkillIconMeta } from '../api/skillIconApi';
import {
  SKILL_DESCRIPTION_MAX_LENGTH,
  SKILL_INSTRUCTIONS_MAX_LENGTH,
  SKILL_NAME_MAX_LENGTH,
  validateSkill,
} from '../lib/skillValidation';
import type { SkillWriteInput } from '../model/types';
import { SkillIconDialog } from './SkillIconDialog';

/**
 * The icon control's binding. Absent means the skill has no version to bind an
 * icon TO — a skill being created, or the AI-generation preview — and the
 * button then says so instead of opening a picker that could not save.
 */
export interface SkillIconControl {
  readonly projectId: string;
  /** The skill VERSION the icon binds to; the PUT addresses it by id. */
  readonly versionId: string;
  readonly iconMeta: SkillIconMeta | null;
  readonly onIconChange: (iconMeta: SkillIconMeta | null) => void;
}

export interface SkillFormProps {
  readonly value: SkillWriteInput;
  readonly onChange: (value: SkillWriteInput) => void;
  readonly disabled?: boolean;
  readonly showErrors?: boolean;
  readonly onGenerate?: () => void;
  readonly icon?: SkillIconControl | undefined;
}

type FieldName = 'name' | 'description' | 'instructions';

function toTags(value: string): readonly string[] {
  return value
    .split(',')
    .map((tag) => tag.trim())
    .filter(Boolean);
}

interface SkillFormHeaderProps {
  readonly disabled: boolean;
  readonly onGenerate: (() => void) | undefined;
}

function SkillFormHeader({ disabled, onGenerate }: SkillFormHeaderProps): ReactNode {
  return (
    <Box sx={headingRowSx}>
      <Typography variant="headingSmall">{t('skills.form.general', 'General')}</Typography>
      {onGenerate && (
        <BaseBtn
          variant="secondary"
          startIcon={<AutoAwesomeIcon />}
          disabled={disabled}
          onClick={onGenerate}
        >
          {t('skills.form.generate', 'Generate with AI')}
        </BaseBtn>
      )}
    </Box>
  );
}

/**
 * SkillIconButton — the control that was disabled behind a "coming soon"
 * tooltip until the `/upload_skill_icon` route family existed. It is enabled
 * exactly when there is a version to bind an icon to; the create form still
 * has none, and it says why rather than opening a picker whose save would 404.
 */
function SkillIconButton({
  icon,
  disabled,
  skillName,
}: {
  readonly icon: SkillIconControl | undefined;
  readonly disabled: boolean;
  readonly skillName: string;
}): ReactNode {
  const [open, setOpen] = useState(false);

  const button = (
    <BaseBtn
      variant="secondary"
      startIcon={
        icon?.iconMeta?.url ? (
          <Box
            component="img"
            src={icon.iconMeta.url}
            alt=""
            sx={iconPreviewSx}
          />
        ) : (
          <ImageOutlinedIcon />
        )
      }
      disabled={disabled || icon === undefined}
      onClick={() => { setOpen(true); }}
      data-testid="skill-icon-button"
    >
      {t('skills.form.icon', 'Icon')}
    </BaseBtn>
  );

  if (icon === undefined) {
    return (
      <Tooltip title={t('skills.form.iconNeedsSave', 'Save the skill first to give it a custom icon.')}>
        <span>{button}</span>
      </Tooltip>
    );
  }

  return (
    <>
      {button}
      <SkillIconDialog
        open={open}
        onClose={() => { setOpen(false); }}
        projectId={icon.projectId}
        versionId={icon.versionId}
        skillName={skillName}
        selectedIcon={icon.iconMeta}
        onIconSelect={icon.onIconChange}
      />
    </>
  );
}

export function SkillForm({
  value,
  onChange,
  disabled = false,
  showErrors = false,
  onGenerate,
  icon,
}: SkillFormProps): ReactNode {
  const [preview, setPreview] = useState(false);
  const errors = useMemo(() => validateSkill(value), [value]);

  const changeField = (field: FieldName) => (event: ChangeEvent<HTMLInputElement>): void => {
    onChange({ ...value, [field]: event.target.value });
  };

  return (
    <Box sx={containerSx}>
      <SkillFormHeader
        disabled={disabled}
        onGenerate={onGenerate}
      />
      <Box sx={nameRowSx}>
        <SkillIconButton
          icon={icon}
          disabled={disabled}
          skillName={value.name}
        />
        <TextField
          fullWidth
          required
          label={t('skills.form.name', 'Name')}
          value={value.name}
          onChange={changeField('name')}
          disabled={disabled}
          error={showErrors && errors.name !== undefined}
          helperText={showErrors ? errors.name : undefined}
          slotProps={{ htmlInput: { 'data-testid': 'skill-name-input' } }}
        />
      </Box>
      <CharacterCounter
        value={value.name}
        maxLength={SKILL_NAME_MAX_LENGTH}
      />
      <TextField
        fullWidth
        required
        multiline
        minRows={2}
        label={t('skills.form.description', 'Description')}
        value={value.description}
        onChange={changeField('description')}
        disabled={disabled}
        error={showErrors && errors.description !== undefined}
        helperText={showErrors ? errors.description : undefined}
        slotProps={{
          htmlInput: { 'data-testid': 'skill-description-input' },
        }}
      />
      <CharacterCounter
        value={value.description}
        maxLength={SKILL_DESCRIPTION_MAX_LENGTH}
      />
      <TextField
        fullWidth
        label={t('skills.form.tags', 'Tags')}
        value={value.tags.join(', ')}
        onChange={(event) => {
          onChange({ ...value, tags: toTags(event.target.value) });
        }}
        disabled={disabled}
        helperText={t('skills.form.tagsHint', 'Separate tags with commas.')}
        slotProps={{ htmlInput: { 'data-testid': 'skill-tags-input' } }}
      />
      <Box sx={headingRowSx}>
        <Typography variant="headingSmall">{t('skills.form.instructions', 'Instructions')}</Typography>
        <BaseBtn
          variant="secondary"
          disabled={disabled && !preview}
          onClick={() => {
            setPreview((current) => !current);
          }}
        >
          {preview ? t('skills.form.edit', 'Edit') : t('skills.form.preview', 'Preview')}
        </BaseBtn>
      </Box>
      {preview ? (
        <Box
          sx={previewSx}
          data-testid="skill-instructions-preview"
        >
          <Markdown>{value.instructions}</Markdown>
        </Box>
      ) : (
        <>
          <TextField
            fullWidth
            required
            multiline
            minRows={12}
            label={t('skills.form.instructions', 'Instructions')}
            value={value.instructions}
            onChange={changeField('instructions')}
            disabled={disabled}
            error={showErrors && errors.instructions !== undefined}
            helperText={showErrors ? errors.instructions : undefined}
            slotProps={{
              htmlInput: {
                maxLength: SKILL_INSTRUCTIONS_MAX_LENGTH,
                'data-testid': 'skill-instructions-input',
              },
            }}
          />
          <CharacterCounter
            value={value.instructions}
            maxLength={SKILL_INSTRUCTIONS_MAX_LENGTH}
          />
        </>
      )}
    </Box>
  );
}

const iconPreviewSx: SxProps<Theme> = {
  width: '1.25rem',
  height: '1.25rem',
  borderRadius: 'var(--el-shape-radiusPill, 9999px)',
  objectFit: 'cover',
};
const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1.5),
  maxWidth: '60rem',
});
const headingRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: 1,
};
const nameRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };
const previewSx: SxProps<Theme> = (theme: Theme) => ({
  minHeight: '12rem',
  padding: theme.spacing(2),
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.background.secondary,
});
