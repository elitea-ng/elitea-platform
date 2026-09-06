import type { ReactNode } from 'react';

import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { ToolListError } from '@/shared/ui/ToolListError';

import type { JsonSchemaLike } from '../../indexes/lib/helpers/indexChat.helpers';
import type { ToolFormContainerProperty, ToolFormContainerSchema } from '../form/ToolFormContainer';
import { ToolFormContainer } from '../form/ToolFormContainer';

/**
 * The argument form of the tool selected in `./TestToolSettings.tsx`, plus
 * the sticky Run Tool button below it (baseline: `TestToolSettings.jsx`'s own
 * `{selectedTool && ...}` block).
 *
 * A LOST SCHEMA READ IS ITS OWN STATE (#440). The static schema tier is a
 * server read — `ListTypeSchemas` merges the SDK argument schemas into each
 * toolkit type — so a lost read leaves no fields to draw, which is the same
 * screen a tool that takes no arguments draws. `schemaRead` carries that
 * outcome and the retry beside it.
 *
 * It lives in its own file, not inside `TestToolSettings.tsx`, to keep both
 * that file under the §3.5 400-line budget and its component under the
 * complexity budget (12).
 */
export interface ToolArgumentPanelProps {
  readonly schema: JsonSchemaLike | null | undefined;
  /** The state of the read that produced `schema`. Absent means the caller resolves the schema without a read. */
  readonly schemaRead?: { readonly isError: boolean; readonly onRetry: () => void } | undefined;
  readonly toolInputVariables: Readonly<Record<string, unknown>>;
  readonly onFieldChange: (fieldKey: string, value: unknown) => void;
  readonly onRunTool: () => void;
  readonly runDisabled: boolean;
}

export function ToolArgumentPanel({ schema, schemaRead, toolInputVariables, onFieldChange, onRunTool, runDisabled }: ToolArgumentPanelProps): ReactNode {
  return (
    <Box sx={configContainerSx}>
      {schemaRead?.isError === true && (
        <ToolListError
          onRetry={schemaRead.onRetry}
          testId="tool-schema-error"
          message={t('features.toolkits.testToolSettings.toolSchemaError', 'The tool settings did not load. Try again.')}
        />
      )}
      <Box sx={scrollableSectionSx}>
        {Object.keys(schema?.properties ?? {}).map((key) => (
          <ToolFormContainer
            key={key}
            fieldKey={key}
            property={schema?.properties?.[key] as ToolFormContainerProperty}
            toolInputVariables={toolInputVariables}
            schema={schema as ToolFormContainerSchema | undefined}
            onChangeInputVariables={onFieldChange}
          />
        ))}
      </Box>
      <Box sx={runToolBtnSx}>
        <BaseBtn
          variant={BUTTON_VARIANTS.special}
          fullWidth
          disabled={runDisabled}
          onClick={onRunTool}
          startIcon={<PlayArrowIcon />}
        >
          {t('features.toolkits.testToolSettings.runTool', 'RUN TOOL')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

const configContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minHeight: '25rem',
};

const scrollableSectionSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  overflowY: 'auto',
  overflowX: 'hidden',
  paddingRight: '.5rem',
  marginRight: '-.5rem',
  '&::-webkit-scrollbar': { width: '.375rem' },
  '&::-webkit-scrollbar-track': { background: 'transparent' },
  '&::-webkit-scrollbar-thumb': { background: theme.vars.palette.divider, borderRadius: theme.vars.shape.radiusSm },
  '&::-webkit-scrollbar-thumb:hover': { background: theme.vars.palette.action.hover },
});

const runToolBtnSx: SxProps<Theme> = (theme) => ({
  marginTop: '1rem',
  paddingRight: '0.5rem',
  paddingTop: '1rem',
  borderTop: `.0625rem solid ${theme.vars.palette.divider}`,
  position: 'sticky',
  bottom: 0,
  zIndex: 1,
});
