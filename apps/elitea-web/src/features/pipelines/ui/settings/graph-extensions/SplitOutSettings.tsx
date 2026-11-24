import type { ReactNode } from 'react';
import { useContext } from 'react';
import Stack from '@mui/material/Stack';
import FormControlLabel from '@mui/material/FormControlLabel';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { extensionRecord, extensionStrings, extensionText, patchExtensionRecord, pointerDefaultPreview } from '../../../lib/graphExtensions.helpers';
import type { ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';
import { StateChannelSelect } from './StateChannelSelect';
import { SplitRetentionFields } from './SplitRetentionFields';
import { ExtensionLimits } from './ExtensionLimits';

function SplitOutBasicFields({
  node,
  disabled,
  change,
  split,
  mode,
  source,
  output,
  preview,
  changeMode,
}: {
  readonly node: ExtensionSettingsProps['node'];
  readonly disabled: boolean;
  readonly change: ExtensionSettingsProps['change'];
  readonly split: Readonly<Record<string, unknown>>;
  readonly mode: string;
  readonly source: string;
  readonly output: string;
  readonly preview: string | undefined;
  readonly changeMode: (next: string) => void;
}): ReactNode {
  return <>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.splitDescription', 'Create one data record per list item. This node does not run a worker.')}</Typography>
    <ExtensionChoice label={t('pipelines.graphExtensions.splitMode', 'List location')} value={mode} disabled={disabled}
      choices={['list', 'row_field', 'rows_field']} change={changeMode} />
    <StateChannelSelect label={t('pipelines.graphExtensions.source', 'Source state variable')} value={source}
      types={mode === 'row_field' ? ['dict'] : ['list']} exclude={output} disabled={disabled} change={(value) => change('source', value)} />
    {mode !== 'list' && <ExtensionField label={t('pipelines.graphExtensions.listPointer', 'List field pointer')}
      value={extensionText(split['path'])} disabled={disabled} change={(path) => change('split', { ...split, path })}
      help={preview ? t('pipelines.graphExtensions.pointerPreview', 'Authored default at this pointer: {{preview}}', { preview })
        : t('pipelines.graphExtensions.pointerHelp', 'Use an RFC 6901 pointer, such as /orders. Escape / as ~1 and ~ as ~0.')} />}
    <ExtensionField label={t('pipelines.graphExtensions.itemField', 'Item field name')} value={extensionText(node['destination'])}
      disabled={disabled} change={(value) => change('destination', value)}
      help={t('pipelines.graphExtensions.itemFieldHelp', 'Use a literal field name inside each data record. Existing retained fields cannot be overwritten.')} />
    <StateChannelSelect label={t('pipelines.graphExtensions.output', 'Output list state variable')} value={output} types={['list']}
      exclude={source} disabled={disabled} change={(value) => change('output', value === '' ? [] : [value])} />
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.splitResult', 'Each result has identity, original ordinals, and data. The output list is replaced once.')}</Typography>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.splitOut.envelopeHelp', 'Each output row is {parent_index, position, data}. data holds the retained fields and the item.')}</Typography>
  </>;
}

function SplitOutAdvancedFields({ props, mode }: { readonly props: ExtensionSettingsProps; readonly mode: string }): ReactNode {
  const { node, disabled, change } = props;
  return <>
    <SplitRetentionFields value={node['retain']} defaultMode="none" choices={mode === 'list' ? ['none'] : ['none', 'all', 'only', 'except']} disabled={disabled} change={(value) => change('retain', value)} />
    <ExtensionChoice label={t('pipelines.graphExtensions.missingList', 'Missing list')} value={extensionText(node['missing_list']) || 'error'}
      disabled={disabled} choices={['error', 'empty']} change={(value) => change('missing_list', value)} />
    <ExtensionChoice label={t('pipelines.graphExtensions.nullList', 'Null list')} value={extensionText(node['null_list']) || 'error'}
      disabled={disabled} choices={['error', 'empty']} change={(value) => change('null_list', value)} />
    <FormControlLabel label={t('pipelines.graphExtensions.removeSource', 'Remove source field after retention')}
      control={<BaseCheckbox className="nodrag nopan nowheel" checked={node['remove_source'] !== false} disabled={disabled}
        onChange={(_, checked) => change('remove_source', checked)} />} />
    <ExtensionLimits {...props} />
  </>;
}

export function SplitOutSettings(props: ExtensionSettingsProps): ReactNode {
  const { node, disabled, change, section = 'all' } = props;
  const showBasic = section === 'all' || section === 'basic';
  const showAdvanced = section === 'all' || section === 'advanced';
  const document = useContext(FlowEditorContext)?.yamlJsonObject;
  const split = extensionRecord(node['split']);
  const mode = extensionText(split['mode']) || 'list';
  const source = extensionText(node['source']);
  const output = extensionStrings(node['output'])[0] ?? '';
  const sourceState =
    document?.state && Object.hasOwn(document.state, source) ? document.state[source] : undefined;
  const preview = pointerDefaultPreview(sourceState, split['path']);
  const changeMode = (next: string) => {
    let updated = patchExtensionRecord(split, 'mode', next);
    if (next === 'list') updated = patchExtensionRecord(updated, 'path', null, true);
    else if (!Object.hasOwn(split, 'path')) updated = patchExtensionRecord(updated, 'path', '');
    change('split', updated);
  };
  return <Stack spacing={2}>
    {showBasic && (
      <SplitOutBasicFields
        node={node}
        disabled={disabled}
        change={change}
        split={split}
        mode={mode}
        source={source}
        output={output}
        preview={preview}
        changeMode={changeMode}
      />
    )}
    {showAdvanced && <SplitOutAdvancedFields props={props} mode={mode} />}
  </Stack>;
}
