import type { ReactNode } from 'react';
import { useContext } from 'react';
import Box from '@mui/material/Box';
import FormHelperText from '@mui/material/FormHelperText';
import { t } from '@/shared/i18n';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';
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
  const options: SingleSelectOption[] = [
    { value: '', label: t('pipelines.graphExtensions.selectChannel', 'Select a state variable') },
    ...(!channels.includes(value) && value !== '' ? [{ value, label: value }] : []),
    ...channels.map((key) => ({ value: key, label: key })),
  ];
  return <Box className="nodrag nopan nowheel">
    <SingleSelect label={label} value={value} disabled={disabled} options={options} onChange={change} />
    <FormHelperText>{help}</FormHelperText>
  </Box>;
}
