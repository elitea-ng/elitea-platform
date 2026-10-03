import type { ReactNode } from 'react';

import { Box, Typography } from '@mui/material';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { DiscardButton } from '@/shared/ui/DiscardButton';

/**
 * Phase-3 Editor header: EditorHeader
 * Stub — no external dependencies. Full implementation wired in Phase 5.
 */
export type EditorHeaderProps = {
  title: string;
  subtitle?: string;
  onCancel: () => void;
  onDiscard: () => void;
  saveButton?: ReactNode;
  isPublic?: boolean;
};

const EditorHeader = ({ title, subtitle, onCancel, onDiscard, saveButton, isPublic }: EditorHeaderProps) => {
  const theme = useTheme();

  return (
    <Box
      sx={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        padding: '0.5rem 1rem',
        borderBottom: '1px solid',
        borderColor: 'border.lines',
        minHeight: '2.625rem',
        background: 'background.userInputBackground',
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.4 }}>
        <Box
          component="span"
          sx={{
            cursor: 'pointer',
            fontSize: theme.typography.headingMedium.fontSize,
            color: theme.vars.palette.icon.fill.default,
          }}
          onClick={onCancel}
        >
          {t('widgets.editor.editorHeader.close', '✕')}
        </Box>
        <Box>
          <Typography variant="headingSmall" component="h6" color="text.secondary" noWrap>
            {title}
          </Typography>
          {subtitle && (
            <Typography variant="bodySmall" component="p" color="text.primary" noWrap>
              {subtitle}
            </Typography>
          )}
        </Box>
      </Box>

      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        {isPublic ? (
          <Box
            sx={{
              boxSizing: 'border-box',
              display: 'flex',
              alignItems: 'center',
              padding: '0.125rem 0.375rem',
              height: '1.25rem',
              borderRadius: theme.vars.shape.radiusPill,
              border: '1px solid',
              borderColor: 'border.lines',
            }}
          >
            <Typography variant="bodySmall" color="text.metrics" sx={{ textTransform: 'none' }}>
              {t('widgets.editor.editorHeader.publicLabel', 'Public')}
            </Typography>
          </Box>
        ) : (
          <>
            <DiscardButton onDiscard={onDiscard} />
            {saveButton}
          </>
        )}
      </Box>
    </Box>
  );
};

EditorHeader.displayName = 'EditorHeader';

export default EditorHeader;
