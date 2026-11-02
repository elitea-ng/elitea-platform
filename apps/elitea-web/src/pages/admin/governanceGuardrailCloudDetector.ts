/**
 * The model of the cloud classifier detector form (ADR-0043 decision 1): the
 * form fields and the detector object the gateway reads
 * (elitea-llm-gateway internal/guardrail/adapter_*.go). Kept out of
 * `GovernanceGuardrailCloudFields.tsx` so that file stays in its length
 * budget.
 */

export type CloudType = 'azure_content_safety' | 'bedrock_guardrails' | 'model_armor';

/** The Azure harm categories, in the order the form shows them. */
export const AZURE_CATEGORIES = ['Hate', 'Sexual', 'Violence', 'SelfHarm'] as const;

/** The form of one cloud detector. Every value is the text of its field. */
export interface CloudDetectorForm {
  readonly type: CloudType;
  readonly name: string;
  readonly action: string;
  readonly endpoint: string;
  // Azure AI Content Safety
  readonly apiKeySecret: string;
  readonly outputType: string;
  readonly thresholds: Readonly<Record<(typeof AZURE_CATEGORIES)[number], string>>;
  readonly blocklists: string;
  readonly promptShields: boolean;
  // AWS Bedrock Guardrails
  readonly region: string;
  readonly guardrailIdentifier: string;
  readonly guardrailVersion: string;
  readonly accessKeyIdSecret: string;
  readonly secretAccessKeySecret: string;
  readonly sessionTokenSecret: string;
  // Google Cloud Model Armor
  readonly template: string;
  readonly authMode: string;
  readonly credentialsSecret: string;
}

export const EMPTY_CLOUD_FORM: CloudDetectorForm = {
  type: 'azure_content_safety',
  name: '',
  action: 'block',
  endpoint: '',
  apiKeySecret: '',
  outputType: 'FourSeverityLevels',
  thresholds: { Hate: '4', Sexual: '4', Violence: '4', SelfHarm: '4' },
  blocklists: '',
  promptShields: true,
  region: '',
  guardrailIdentifier: '',
  guardrailVersion: '',
  accessKeyIdSecret: '',
  secretAccessKeySecret: '',
  sessionTokenSecret: '',
  template: '',
  authMode: 'key',
  credentialsSecret: '',
};

function setIf(target: Record<string, unknown>, key: string, value: string): void {
  const trimmed = value.trim();
  if (trimmed !== '') target[key] = trimmed;
}

/** The detector object of a form, as the gateway reads it. An empty field is omitted. */
export function buildCloudDetector(form: CloudDetectorForm): Record<string, unknown> {
  const detector: Record<string, unknown> = {
    type: form.type,
    action: form.action,
  };
  setIf(detector, 'name', form.name);
  setIf(detector, 'endpoint', form.endpoint);
  if (form.type === 'azure_content_safety') {
    setIf(detector, 'api_key_secret', form.apiKeySecret);
    if (form.outputType !== 'FourSeverityLevels') detector.output_type = form.outputType;
    const categories: Record<string, number> = {};
    for (const name of AZURE_CATEGORIES) {
      const value = Number.parseInt(form.thresholds[name], 10);
      if (Number.isFinite(value)) categories[name] = value;
    }
    if (Object.keys(categories).length > 0) detector.categories = categories;
    const blocklists = form.blocklists
      .split(',')
      .map((entry) => entry.trim())
      .filter((entry) => entry !== '');
    if (blocklists.length > 0) detector.blocklists = blocklists;
    if (form.promptShields) detector.prompt_shields = true;
  } else if (form.type === 'bedrock_guardrails') {
    setIf(detector, 'region', form.region);
    setIf(detector, 'guardrail_identifier', form.guardrailIdentifier);
    setIf(detector, 'guardrail_version', form.guardrailVersion);
    setIf(detector, 'access_key_id_secret', form.accessKeyIdSecret);
    setIf(detector, 'secret_access_key_secret', form.secretAccessKeySecret);
    setIf(detector, 'session_token_secret', form.sessionTokenSecret);
  } else {
    setIf(detector, 'template', form.template);
    detector.auth_mode = form.authMode;
    if (form.authMode === 'key') setIf(detector, 'credentials_secret', form.credentialsSecret);
  }
  return detector;
}

/** The keys of the form whose value is text. */
export type CloudTextKey = {
  [K in keyof CloudDetectorForm]: CloudDetectorForm[K] extends string ? K : never;
}[keyof CloudDetectorForm];
