import { useContext, type ReactNode } from 'react';
import Checkbox from '@mui/material/Checkbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { updateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';

interface Props { readonly id: string; readonly disabled: boolean; }
/** Only an explicit edit writes Debug. Omission and stored values remain unchanged on mount. */
export function CodeDebugSettings({ id, disabled }: Props): ReactNode {
  const context = useContext(FlowEditorContext);
  const node = context?.yamlJsonObject.nodes?.find((candidate) => candidate.id === id);
  if (!context || !node || (node.type !== undefined && node.type.toLowerCase() !== 'code')) return null;
  return <Stack className="nodrag nopan" spacing={0.5}>
    <FormControlLabel label={t('pipelines.flowEditor.codeNode.debug', 'Debug')}
      control={<Checkbox checked={node['debug'] === true} disabled={disabled}
        onChange={(_, checked) => {
          if (!disabled) updateYamlNode(id, 'debug', checked, context.yamlJsonObject, context.setYamlJsonObject);
        }} />} />
    <Typography variant="bodySmall" color="text.secondary">
      {t('pipelines.flowEditor.codeNode.debugHelp', 'Save executable Code source and selected input state in a debug snapshot. Artifact permissions control export and download.')}
    </Typography>
  </Stack>;
}
