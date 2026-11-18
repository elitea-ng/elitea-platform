/**
 * The cloud classifier detectors of a content guardrail (ADR-0043 decision 1):
 * Azure AI Content Safety, AWS Bedrock Guardrails and Google Cloud Model
 * Armor. The admin fills the fields of one detector and adds it to the
 * detectors JSON of the entry. The server checks every key on write
 * (elitea-main governance_guardrail_cloud.go).
 *
 * Credentials are always {{secret.NAME}} references to the public project's
 * secrets vault, never raw values.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { parseGuardrailDetectors } from './governanceGuardrailDraft';
import {
  AZURE_CATEGORIES,
  EMPTY_CLOUD_FORM,
  buildCloudDetector,
  type CloudDetectorForm,
  type CloudTextKey,
  type CloudType,
} from './governanceGuardrailCloudDetector';
import type { GovernanceDraft } from './useGatewayGovernancePage';

interface CloudFieldsProps {
  readonly draft: GovernanceDraft;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}

interface CloudFieldProps {
  readonly form: CloudDetectorForm;
  readonly onSet: (patch: Partial<CloudDetectorForm>) => void;
  readonly name: CloudTextKey;
  readonly label: string;
  readonly testId: string;
  readonly help?: string;
}

/** One text field of the cloud detector form. */
function CloudField({ form, onSet, name, label, testId, help }: CloudFieldProps) {
  return (
    <TextField
      label={label}
      helperText={help}
      value={form[name]}
      onChange={(event) => onSet({ [name]: event.target.value })}
      size="small"
      fullWidth
      slotProps={{ htmlInput: { 'data-testid': testId, spellCheck: false } }}
    />
  );
}

/** A form that adds one cloud classifier detector to the detectors JSON of the draft. */
export function GuardrailCloudFields({ draft, onChange }: CloudFieldsProps) {
  const [form, setForm] = useState<CloudDetectorForm>(EMPTY_CLOUD_FORM);
  const [error, setError] = useState<string | undefined>(undefined);
  const set = (patch: Partial<CloudDetectorForm>) => setForm((current) => ({ ...current, ...patch }));
  const secretHelp = t(
    'pages.admin.governance.cloud.secretHelp',
    'A {{ref}} reference to a secret of the public project. A raw value is refused.',
    { ref: '{{secret.NAME}}' },
  );

  const add = () => {
    const parsed = parseGuardrailDetectors(draft.guardrailDetectors);
    if ('error' in parsed) {
      setError(parsed.error);
      return;
    }
    setError(undefined);
    onChange({
      guardrailDetectors: JSON.stringify([...parsed.list, buildCloudDetector(form)], null, 2),
    });
  };

  return (
    <Stack spacing={1.5} data-testid="governance-guardrail-cloud">
      <Typography variant="labelMedium">
        {t('pages.admin.governance.cloud.title', 'Add a cloud classifier detector')}
      </Typography>
      <TextField
        select
        label={t('pages.admin.governance.cloud.type', 'Classifier')}
        value={form.type}
        onChange={(event) => set({ type: event.target.value as CloudType })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-cloud-type' } }}
      >
        <MenuItem value="azure_content_safety">
          {t('pages.admin.governance.cloud.typeAzure', 'Azure AI Content Safety')}
        </MenuItem>
        <MenuItem value="bedrock_guardrails">
          {t('pages.admin.governance.cloud.typeBedrock', 'AWS Bedrock Guardrails')}
        </MenuItem>
        <MenuItem value="model_armor">
          {t('pages.admin.governance.cloud.typeModelArmor', 'Google Cloud Model Armor')}
        </MenuItem>
      </TextField>
      <CloudField
        form={form}
        onSet={set}
        name="name"
        label={t('pages.admin.governance.cloud.name', 'Detector name')}
        testId="governance-cloud-name"
      />
      <TextField
        select
        label={t('pages.admin.governance.cloud.action', 'Action')}
        value={form.action}
        onChange={(event) => set({ action: event.target.value })}
        size="small"
        fullWidth
      >
        <MenuItem value="block">{t('pages.admin.governance.cloud.actionBlock', 'Block')}</MenuItem>
        <MenuItem value="mask">{t('pages.admin.governance.cloud.actionMask', 'Mask')}</MenuItem>
        <MenuItem value="flag">{t('pages.admin.governance.cloud.actionFlag', 'Flag')}</MenuItem>
        <MenuItem value="warn">{t('pages.admin.governance.cloud.actionWarn', 'Warn')}</MenuItem>
      </TextField>

      {form.type === 'azure_content_safety' && (
        <>
          <CloudField
            form={form}
            onSet={set}
            name="endpoint"
            label={t('pages.admin.governance.cloud.azureEndpoint', 'Resource endpoint')}
            testId="governance-cloud-endpoint"
            help={t(
              'pages.admin.governance.cloud.azureEndpointHelp',
              'For example https://NAME.cognitiveservices.azure.com.',
            )}
          />
          <CloudField
            form={form}
            onSet={set}
            name="apiKeySecret"
            label={t('pages.admin.governance.cloud.azureKey', 'Resource key secret')}
            testId="governance-cloud-azure-key"
            help={secretHelp}
          />
          <TextField
            select
            label={t('pages.admin.governance.cloud.azureOutputType', 'Severity scale')}
            value={form.outputType}
            onChange={(event) => set({ outputType: event.target.value })}
            size="small"
            fullWidth
          >
            <MenuItem value="FourSeverityLevels">
              {t('pages.admin.governance.cloud.azureFour', 'Four levels (0, 2, 4, 6)')}
            </MenuItem>
            <MenuItem value="EightSeverityLevels">
              {t('pages.admin.governance.cloud.azureEight', 'Eight levels (0 to 7)')}
            </MenuItem>
          </TextField>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
            {AZURE_CATEGORIES.map((name) => (
              <TextField
                key={name}
                label={name}
                value={form.thresholds[name]}
                onChange={(event) =>
                  set({
                    thresholds: {
                      ...form.thresholds,
                      [name]: event.target.value,
                    },
                  })
                }
                size="small"
                fullWidth
                slotProps={{
                  htmlInput: {
                    inputMode: 'numeric',
                    'data-testid': `governance-cloud-azure-${name}`,
                  },
                }}
              />
            ))}
          </Stack>
          <Typography variant="bodySmall" color="text.secondary">
            {t(
              'pages.admin.governance.cloud.azureThresholdHelp',
              'The lowest severity that is a finding, per category. Empty skips the category.',
            )}
          </Typography>
          <CloudField
            form={form}
            onSet={set}
            name="blocklists"
            label={t('pages.admin.governance.cloud.azureBlocklists', 'Blocklists')}
            testId="governance-cloud-azure-blocklists"
            help={t(
              'pages.admin.governance.cloud.azureBlocklistsHelp',
              'Comma-separated blocklist names of the resource.',
            )}
          />
          <FormControlLabel
            control={
              <Switch
                checked={form.promptShields}
                onChange={(event) => set({ promptShields: event.target.checked })}
                data-testid="governance-cloud-azure-shields"
              />
            }
            label={t('pages.admin.governance.cloud.azureShields', 'Prompt Shields (label prompt_injection)')}
          />
        </>
      )}

      {form.type === 'bedrock_guardrails' && (
        <>
          <CloudField
            form={form}
            onSet={set}
            name="region"
            label={t('pages.admin.governance.cloud.bedrockRegion', 'AWS region')}
            testId="governance-cloud-bedrock-region"
          />
          <CloudField
            form={form}
            onSet={set}
            name="guardrailIdentifier"
            label={t('pages.admin.governance.cloud.bedrockId', 'Guardrail id or ARN')}
            testId="governance-cloud-bedrock-id"
          />
          <CloudField
            form={form}
            onSet={set}
            name="guardrailVersion"
            label={t('pages.admin.governance.cloud.bedrockVersion', 'Guardrail version')}
            testId="governance-cloud-bedrock-version"
            help={t('pages.admin.governance.cloud.bedrockVersionHelp', 'A version number, or DRAFT.')}
          />
          <CloudField
            form={form}
            onSet={set}
            name="accessKeyIdSecret"
            label={t('pages.admin.governance.cloud.bedrockKeyId', 'Access key id secret')}
            testId="governance-cloud-bedrock-key-id"
            help={secretHelp}
          />
          <CloudField
            form={form}
            onSet={set}
            name="secretAccessKeySecret"
            label={t('pages.admin.governance.cloud.bedrockSecretKey', 'Secret access key secret')}
            testId="governance-cloud-bedrock-secret-key"
            help={secretHelp}
          />
          <CloudField
            form={form}
            onSet={set}
            name="sessionTokenSecret"
            label={t('pages.admin.governance.cloud.bedrockToken', 'Session token secret (optional)')}
            testId="governance-cloud-bedrock-token"
          />
          <CloudField
            form={form}
            onSet={set}
            name="endpoint"
            label={t('pages.admin.governance.cloud.bedrockEndpoint', 'Endpoint (optional)')}
            testId="governance-cloud-endpoint"
            help={t(
              'pages.admin.governance.cloud.bedrockEndpointHelp',
              'Empty means https://bedrock-runtime.REGION.amazonaws.com. Set it for a VPC endpoint.',
            )}
          />
        </>
      )}

      {form.type === 'model_armor' && (
        <>
          <CloudField
            form={form}
            onSet={set}
            name="template"
            label={t('pages.admin.governance.cloud.armorTemplate', 'Template')}
            testId="governance-cloud-armor-template"
            help={t(
              'pages.admin.governance.cloud.armorTemplateHelp',
              'projects/PROJECT/locations/LOCATION/templates/TEMPLATE',
            )}
          />
          <TextField
            select
            label={t('pages.admin.governance.cloud.armorAuth', 'Credentials')}
            value={form.authMode}
            onChange={(event) => set({ authMode: event.target.value })}
            size="small"
            fullWidth
            slotProps={{
              htmlInput: { 'data-testid': 'governance-cloud-armor-auth' },
            }}
          >
            <MenuItem value="key">{t('pages.admin.governance.cloud.armorAuthKey', 'Service-account key')}</MenuItem>
            <MenuItem value="workload_identity">
              {t('pages.admin.governance.cloud.armorAuthKeyless', 'Gateway workload identity (keyless)')}
            </MenuItem>
          </TextField>
          {form.authMode === 'key' && (
            <CloudField
              form={form}
              onSet={set}
              name="credentialsSecret"
              label={t('pages.admin.governance.cloud.armorKey', 'Service-account key secret')}
              testId="governance-cloud-armor-key"
              help={secretHelp}
            />
          )}
          <CloudField
            form={form}
            onSet={set}
            name="endpoint"
            label={t('pages.admin.governance.cloud.armorEndpoint', 'Endpoint (optional)')}
            testId="governance-cloud-endpoint"
            help={t(
              'pages.admin.governance.cloud.armorEndpointHelp',
              'Empty means https://modelarmor.LOCATION.rep.googleapis.com.',
            )}
          />
        </>
      )}

      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <Button variant="outlined" size="small" onClick={add} data-testid="governance-cloud-add">
          {t('pages.admin.governance.cloud.add', 'Add to detectors')}
        </Button>
        {error !== undefined && (
          <Typography variant="bodySmall" color="error">
            {error}
          </Typography>
        )}
      </Box>
    </Stack>
  );
}
