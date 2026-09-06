import { memo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import ModelCapabilitiesSection from './ModelCapabilitiesSection';

/**
 * The model picker and the capability chips it describes — the last piece of
 * the baseline's `ModelConfiguration` level (issue #80, item 4).
 *
 * The chips alone were mounted, and they described the project's DEFAULT
 * model because nothing could select any other. The baseline has the same
 * limit; the issue asks for the picker so the section answers "what can THIS
 * model do?" for every model in the catalogue, not only the default one.
 *
 * ONE COMPONENT, not a picker in the page and chips beside it. The page
 * mounts this and nothing else, so the two cannot come apart: a picker whose
 * value the chips did not read would look right and mean nothing, which is
 * the exact failure class #80 belongs to.
 *
 * Rendering nothing when there is no model to describe is deliberate. An
 * empty capability list is the catalogue saying "this model declares none",
 * and a picker over an empty catalogue offers nothing to pick — in both cases
 * the row would cost header-sized space to say nothing.
 */
/* Not exported: `ProjectAIConfiguration` and `ModelCapabilitiesSection` keep
   their prop types local for the same reason — the page composes this through
   `aiConfigurationFeature`, and knip flags an unused named export. */
interface ModelCapabilitiesPanelProps {
  readonly capabilities: readonly string[];
  readonly modelOptions: readonly { readonly value: string; readonly label: string }[];
  readonly selectedModel: string;
  readonly onSelectModel: (value: string) => void;
}

/*
 * The row's own chrome, carried here rather than in a wrapper the page
 * renders. The page used to guard that wrapper with its own copy of "is there
 * anything to show", and a second copy of that rule is how a padded empty
 * strip appears the moment the two disagree. One component, one rule.
 */
const rowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
  flexShrink: 0,
  padding: '0 1.5rem 1rem',
  backgroundColor: theme.vars.palette.background.eliteaDefault,
});

const selectSx: SxProps<Theme> = { maxWidth: '20rem' };

export default memo(function ModelCapabilitiesPanel({
  capabilities,
  modelOptions,
  selectedModel,
  onSelectModel,
}: ModelCapabilitiesPanelProps) {
  if (modelOptions.length === 0 && capabilities.length === 0) return null;

  return (
    <Box
      sx={rowSx}
      data-testid="model-capabilities-panel"
    >
      {modelOptions.length > 0 && (
        <SingleSelect
          value={selectedModel}
          onChange={onSelectModel}
          options={[...modelOptions]}
          label={t('ai-configuration.modelCapabilities.modelLabel', 'Model')}
          placeholder={t('ai-configuration.modelCapabilities.modelPlaceholder', 'Select a model')}
          id="ai-configuration-capability-model"
          name="capability-model"
          sx={selectSx}
        />
      )}
      <ModelCapabilitiesSection capabilities={capabilities} />
    </Box>
  );
});
