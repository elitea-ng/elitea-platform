import type { ReactNode } from 'react';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { extensionStrings, extensionText } from '../../../lib/graphExtensions.helpers';
import type { ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { ExtensionChoice } from './ExtensionFields';
import { StateChannelSelect } from './StateChannelSelect';
import { AggregateGrouping } from './AggregateGrouping';
import { AggregateOperations } from './AggregateOperations';
import { ExtensionLimits } from './ExtensionLimits';

export function AggregateSettings(props: ExtensionSettingsProps): ReactNode {
  const { node, disabled, change } = props;
  const source = extensionText(node['source']);
  const output = extensionStrings(node['output'])[0] ?? '';
  return <Stack spacing={2}>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.aggregateDescription', 'Group list rows and calculate explicit results. Each result keeps group keys and operation values separate.')}</Typography>
    <StateChannelSelect label={t('pipelines.graphExtensions.source', 'Source state variable')} value={source} types={['list']}
      exclude={output} disabled={disabled} change={(value) => change('source', value)} />
    <ExtensionChoice label={t('pipelines.graphExtensions.layout', 'Input row layout')} value={extensionText(node['layout']) || 'plain'}
      choices={['plain', 'split_out']} disabled={disabled} change={(value) => change('layout', value)}
      help={t('pipelines.graphExtensions.layoutHelp', 'plain requires object rows. split_out reads each envelope data object. Nested fields can have unknown shapes.')}/>
    <ExtensionChoice label={t('pipelines.graphExtensions.regroup', 'Regroup')} value={extensionText(node['regroup']) || 'none'}
      choices={['none', 'parent']} disabled={disabled} change={(value) => change('regroup', value)}
      help={t('pipelines.graphExtensions.regroupHelp', 'Parent rebuilds each split parent row from the split_out envelopes.')}/>
    <AggregateGrouping value={node['group_by']} disabled={disabled} change={(value) => change('group_by', value)} />
    <AggregateOperations value={node['operations']} disabled={disabled} change={(value) => change('operations', value)} />
    <StateChannelSelect label={t('pipelines.graphExtensions.output', 'Output list state variable')} value={output} types={['list']}
      exclude={source} disabled={disabled} change={(value) => change('output', value === '' ? [] : [value])} />
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.aggregateResult', 'Replace the output list once after all groups succeed. Integer operations require integer values at runtime.')}</Typography>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.aggregateEmptyHelp', 'Empty input gives one row (count 0, lists [], sum 0, others null) without Group by, and no rows with Group by.')}</Typography>
    <ExtensionLimits {...props} />
  </Stack>;
}
