/**
 * EditorSection — the project context editor with toolbar, preview, and char counter.
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { Markdown } from '@/shared/ui/Markdown';
import { GenerateProjectContextButton } from './GenerateProjectContextButton';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { ImportIcon } from '@/shared/ui/icons/import-icon';
import { t } from '@/shared/i18n';
import { projectContextStyles } from './ProjectContext.styles';
import { memo, useMemo } from 'react';

const MAX_CHARS = 2500;

export interface EditorSectionProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
  content: string;
  mode: 'edit' | 'preview';
  isEditorFocused: boolean;
  showEditorControls: boolean;
  canEdit: boolean;
  onContentChange: (val: string) => void;
  onModeChange: (e: React.SyntheticEvent, newValue: 'edit' | 'preview') => void;
  onFocus: () => void;
  onBlur: (e: React.FocusEvent) => void;
  /**
   * The "Generate with AI" affordance, as ONE prop rather than two.
   *
   * `onApply` was `onAIGenerated`; `openOnMount` is new, and it is how the
   * empty state's "Build with AI" button reaches the dialog (the reference
   * does it with router state, `onNavigate('create', { openAi: true })`).
   * They are grouped because adding a thirteenth flat prop here breaks §3.5's
   * 12-prop component budget, and because the two only ever travel together.
   */
  generate: {
    onApply: (generatedContent: string) => void;
    openOnMount?: boolean;
  };
  onImportClick: () => void;
}

export const EditorSection = memo(function EditorSection({
  projectId,
  content,
  mode,
  isEditorFocused,
  showEditorControls,
  canEdit,
  onContentChange,
  onModeChange,
  onFocus,
  onBlur,
  generate,
  onImportClick,
}: EditorSectionProps) {
  const s = projectContextStyles.editor();
  const limitReached = content.length >= MAX_CHARS;

  const modeButtons = useMemo(
    () => [
      { value: 'edit' as const, label: t('entities.projectContext.content.editMode', 'Edit mode') },
      { value: 'preview' as const, label: t('entities.projectContext.content.previewMode', 'Preview mode') },
    ],
    [],
  );

  return (
    <Box sx={s.section}>
      <Box sx={s.header}>
        <Box sx={s.textBlock}>
          <Typography variant="labelMedium" color="text.secondary">
            {t('entities.projectContext.content.backgroundLabel', 'Project Background')}
          </Typography>
          <Typography variant="bodySmall">
            {t('entities.projectContext.content.backgroundDescription', 'Include goals, terminology, workflows, or constraints relevant to the project.')}
          </Typography>
        </Box>
        {showEditorControls && (
          <Box sx={s.toolbar}>
            <GenerateProjectContextButton
              projectId={projectId}
              existingContent={content}
              onApply={generate.onApply}
              openAiOnMount={generate.openOnMount ?? false}
            />
            <BaseBtn
              variant="secondary"
              size="small"
              startIcon={<ImportIcon />}
              onClick={onImportClick}
              title={t('entities.projectContext.content.importTitle', 'Import markdown file')}
              aria-label={t('entities.projectContext.content.importTitle', 'Import markdown file')}
            />
            <BaseTabs value={mode} onChange={onModeChange}>
              {modeButtons.map((btn) => (
                <BaseTab key={btn.value} value={btn.value} label={btn.label} />
              ))}
            </BaseTabs>
          </Box>
        )}
      </Box>

      {mode === 'edit' ? (
        <Box sx={s.wrapper} onFocus={onFocus} onBlur={onBlur}>
          <CodeMirrorEditor
            value={content}
            onChange={onContentChange}
            height="100%"
            minHeight="0"
            maxLength={MAX_CHARS}
            readOnly={!canEdit}
          />
        </Box>
      ) : (
        <Box sx={s.preview}>
          <Markdown renderHtml={false}>{content}</Markdown>
        </Box>
      )}

      {showEditorControls && (
        <Typography
          variant="bodySmall"
          sx={{
            color: limitReached ? 'error.main' : 'text.primary',
            visibility: isEditorFocused ? 'visible' : 'hidden',
            textAlign: 'right',
          }}
        >
          {MAX_CHARS - content.length}{' '}
          {t('entities.projectContext.content.charactersLeft', 'characters left.')}
          {limitReached && t('entities.projectContext.content.maxReached', 'You have reached the maximum character limit.')}
        </Typography>
      )}
    </Box>
  );
});
