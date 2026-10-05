import type { ReactNode } from 'react';
import { useContext } from 'react';
import TextField from '@mui/material/TextField';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { channelDefaultPreview, declaredChannelType, orderedDeclaredChannels } from '../../../lib/graphExtensions.helpers';
import { BUILTIN_STATE_KEYS } from '../../../lib/graphAdmission.nodeReads';
import { RuntimeContractConstants } from '../../../lib/flow-editor/constants';
import { usePipelineYamlStore } from '../../../model/pipelineYamlStore';

interface StateChannelSelectProps {
  readonly label: string;
  readonly value: string;
  readonly types: readonly string[];
  readonly disabled: boolean;
  readonly change: (value: string) => void;
  readonly exclude?: string;
}
export function StateChannelSelect({ label, value, types, disabled, change, exclude }: StateChannelSelectProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const order = usePipelineYamlStore((state) => state.stateKeyOrder);
  const document = context?.yamlJsonObject ?? {};
  const channels = orderedDeclaredChannels(document, order).filter((key) => key !== exclude
    && !BUILTIN_STATE_KEYS.has(key) && !RuntimeContractConstants.isReservedStateKey(key) && types.includes(declaredChannelType(document.state?.[key])));
  const preview = channelDefaultPreview(document.state?.[value]);
  const help = preview
    ? t('pipelines.graphExtensions.statePreview', 'Authored default: {{preview}}. Nested shape remains unknown.', { preview })
    : t('pipelines.graphExtensions.unknownShape', 'Nested values can have unknown shapes. Declare the root type in State.');
  return <TextField className="nodrag nopan nowheel" select label={label} value={value} disabled={disabled}
    onChange={(event) => change(event.target.value)} helperText={help} size="small" fullWidth
    slotProps={{ select: { native: true } }}>
    <option value="">{t('pipelines.graphExtensions.selectChannel', 'Select a state variable')}</option>
    {!channels.includes(value) && value !== '' && <option value={value}>{value}</option>}
    {channels.map((key) => <option key={key} value={key}>{key}</option>)}
  </TextField>;
}
