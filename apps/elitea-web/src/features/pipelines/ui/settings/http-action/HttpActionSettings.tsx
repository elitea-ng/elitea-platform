import type { ReactNode } from 'react';
import { useContext, useState } from 'react';

import Box from '@mui/material/Box';
import FormHelperText from '@mui/material/FormHelperText';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import { useListHttpActionRuleNames } from '@/shared/api/generated/httpactions/httpactions';
import type { HttpActionRuleName } from '@/shared/api/generated/model';
import { unwrapList } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';

import { RuntimeContractConstants } from '../../../lib/flow-editor/constants';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { useSelectedProjectId } from '../../../lib/flow-editor/hooks/useSelectedProjectId';
import { BUILTIN_STATE_KEYS } from '../../../lib/graphAdmission.nodeReads';
import { extensionStrings, extensionText, orderedDeclaredChannels } from '../../../lib/graphExtensions.helpers';
import type { ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { usePipelineYamlStore } from '../../../model/pipelineYamlStore';
import { StateChannelSelect } from '../graph-extensions/StateChannelSelect';
import { HttpRequestBindingsEditor } from './HttpRequestBindingsEditor';
import { asRecord, formatAcceptedStatuses, parseAcceptedStatuses } from './httpAction.helpers';

const RESPONSE_MODE_OPTIONS: readonly SingleSelectOption[] = [
  { value: 'json', label: 'JSON' },
  { value: 'text', label: 'Text' },
  { value: 'empty', label: 'Empty' },
];

const MAX_RESPONSE_BYTES = 2_097_152;
const MIN_HTTP_TIMEOUT_MS = 1_000;
const MAX_HTTP_TIMEOUT_MS = 30_000;

function buildOperationOptions(ruleItems: readonly HttpActionRuleName[], operation: string): SingleSelectOption[] {
  const hasCurrent = operation === '' || ruleItems.some((item) => item.name === operation);
  return [
    { value: '', label: t('pipelines.httpAction.selectOperation', 'Select an HTTP action rule') },
    ...(hasCurrent ? [] : [{ value: operation, label: operation }]),
    ...ruleItems.map((item) => ({
      value: item.name,
      label: item.enabled
        ? item.name
        : t('pipelines.httpAction.disabledRuleOption', '{{name}} (disabled)', { name: item.name }),
    })),
  ];
}

function parseBoundedInt(raw: string, min: number, max: number): number | 'empty' | 'invalid' {
  const trimmed = raw.trim();
  if (trimmed === '') return 'empty';
  const parsed = Number(trimmed);
  return Number.isSafeInteger(parsed) && parsed >= min && parsed <= max ? parsed : 'invalid';
}

interface ResponseContractFieldsProps {
  readonly response: Readonly<Record<string, unknown>>;
  readonly topTimeout: number | '';
  readonly disabled: boolean;
  readonly onPatchResponse: (patch: Record<string, unknown>) => void;
  readonly onTimeoutChange: (raw: string) => void;
}

function ResponseContractFields({
  response,
  topTimeout,
  disabled,
  onPatchResponse,
  onTimeoutChange,
}: ResponseContractFieldsProps): ReactNode {
  const responseMode = extensionText(response['mode']) || 'json';
  const maxBytes = typeof response['max_bytes'] === 'number' ? response['max_bytes'] : '';
  const [statusesDraft, setStatusesDraft] = useState<string>(() => formatAcceptedStatuses(response['accepted_statuses']));

  const handleStatusesChange = (raw: string): void => {
    setStatusesDraft(raw);
    const parsed = parseAcceptedStatuses(raw);
    if (parsed !== undefined) onPatchResponse({ accepted_statuses: parsed });
  };

  const handleMaxBytesChange = (raw: string): void => {
    const parsed = parseBoundedInt(raw, 1, MAX_RESPONSE_BYTES);
    if (parsed === 'empty') onPatchResponse({ max_bytes: undefined });
    else if (parsed !== 'invalid') onPatchResponse({ max_bytes: parsed });
  };

  return (
    <Stack spacing={1.5}>
      <Typography variant="labelSmall" color="text.secondary">
        {t('pipelines.httpAction.responseContract', 'Response contract')}
      </Typography>
      <SingleSelect
        label={t('pipelines.httpAction.responseMode', 'Response mode')}
        value={responseMode}
        disabled={disabled}
        options={[...RESPONSE_MODE_OPTIONS]}
        onChange={(value) => onPatchResponse({ mode: value })}
      />
      <InputBase
        size="small"
        fullWidth
        label={t('pipelines.httpAction.acceptedStatuses', 'Accepted HTTP statuses')}
        value={statusesDraft}
        disabled={disabled}
        helperText={t('pipelines.httpAction.acceptedStatusesHelp', 'Comma-separated codes or ranges (e.g. 200-299, 404)')}
        onChange={(event) => handleStatusesChange(event.target.value)}
      />
      <InputBase
        size="small"
        fullWidth
        type="number"
        label={t('pipelines.httpAction.maxBytes', 'Max response bytes')}
        value={maxBytes}
        disabled={disabled}
        helperText={t('pipelines.httpAction.maxBytesHelp', '1 to 2097152 bytes (default 1048576)')}
        onChange={(event) => handleMaxBytesChange(event.target.value)}
      />
      <InputBase
        size="small"
        fullWidth
        type="number"
        label={t('pipelines.httpAction.timeoutMs', 'Request timeout (ms)')}
        value={topTimeout}
        disabled={disabled}
        helperText={t('pipelines.httpAction.timeoutMsHelp', 'Optional per-attempt cap (1000 to 30000 ms)')}
        onChange={(event) => onTimeoutChange(event.target.value)}
      />
    </Stack>
  );
}

export function HttpActionSettings({ node, disabled, change, remove }: ExtensionSettingsProps): ReactNode {
  const projectId = useSelectedProjectId();
  const context = useContext(FlowEditorContext);
  const stateKeyOrder = usePipelineYamlStore((state) => state.stateKeyOrder);
  const rulesQuery = useListHttpActionRuleNames(projectId ?? '', {
    query: { enabled: Boolean(projectId) },
  });

  const ruleItems = unwrapList<HttpActionRuleName>(rulesQuery.data, 'HttpActionSettings.ruleNames');
  const operation = extensionText(node['operation']);
  const output = extensionStrings(node['output'])[0] ?? '';
  const response = asRecord(node['response']);
  const topTimeout = typeof node['timeout_ms'] === 'number' ? node['timeout_ms'] : '';

  const document = context?.yamlJsonObject ?? {};
  const stateKeys = orderedDeclaredChannels(document, stateKeyOrder).filter(
    (key) => !BUILTIN_STATE_KEYS.has(key) && !RuntimeContractConstants.isReservedStateKey(key),
  );

  const selectedRule = ruleItems.find((item) => item.name === operation);
  const operationOptions = buildOperationOptions(ruleItems, operation);

  const patchResponse = (patch: Record<string, unknown>): void => {
    const next: Record<string, unknown> = { ...response, ...patch };
    for (const [k, v] of Object.entries(next)) {
      if (v === undefined) delete next[k];
    }
    change('response', next);
  };

  const handleTimeoutChange = (raw: string): void => {
    const parsed = parseBoundedInt(raw, MIN_HTTP_TIMEOUT_MS, MAX_HTTP_TIMEOUT_MS);
    if (parsed === 'empty') remove('timeout_ms');
    else if (parsed !== 'invalid') change('timeout_ms', parsed);
  };

  const isDisabledRule = selectedRule !== undefined && !selectedRule.enabled;

  return (
    <Stack spacing={2} className="nodrag nopan nowheel" data-testid="pipelines-http-action-settings">
      <Typography variant="bodySmall">
        {t(
          'pipelines.httpAction.description',
          'Invoke a project HTTP action rule using typed state bindings. Rule origin, HTTP method, and credentials are managed under Settings → HTTP actions.',
        )}
      </Typography>

      <Box>
        <SingleSelect
          label={t('pipelines.httpAction.operation', 'Operation (rule name)')}
          value={operation}
          disabled={disabled}
          options={operationOptions}
          onChange={(value) => change('operation', value)}
        />
        <FormHelperText>
          {isDisabledRule
            ? t(
                'pipelines.httpAction.ruleDisabledWarning',
                'This HTTP action rule is currently disabled in project settings.',
              )
            : t(
                'pipelines.httpAction.operationHelp',
                'Only rule names and enabled status are exposed to pipeline authors.',
              )}
        </FormHelperText>
      </Box>

      <HttpRequestBindingsEditor
        request={node['request']}
        stateKeys={stateKeys}
        disabled={disabled}
        onChange={(nextRequest) => change('request', nextRequest)}
      />

      <ResponseContractFields
        response={response}
        topTimeout={topTimeout}
        disabled={disabled}
        onPatchResponse={patchResponse}
        onTimeoutChange={handleTimeoutChange}
      />

      <StateChannelSelect
        label={t('pipelines.httpAction.output', 'Output state variable')}
        value={output}
        types={['dict', 'str']}
        disabled={disabled}
        change={(value) => change('output', value === '' ? [] : [value])}
      />
    </Stack>
  );
}
