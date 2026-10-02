/** Credential-free challenge for one saved toolkit. */
export interface ToolkitTestAuthorization {
  readonly toolkit_id: number;
  readonly toolkit_name: string;
  readonly toolkit_type: string;
  readonly server_url: string;
  readonly resource_metadata_url?: string;
  readonly resource_metadata: Readonly<Record<string, unknown>>;
}

function matchesToolkit(identity: unknown, toolkitId: string | number | undefined): boolean {
  const numeric = typeof identity === 'number' && Number.isSafeInteger(identity);
  const decimal = typeof identity === 'string' && /^[1-9][0-9]*$/.test(identity);
  return (numeric || decimal) && Number(identity) > 0 && Number(identity) <= 2147483647 && String(identity) === String(toolkitId);
}

function validResource(value: unknown): value is string {
  if (typeof value !== 'string' || value.length > 2048) return false;
  try {
    const url = new URL(value);
    return ['https:', 'http:'].includes(url.protocol) && !url.username && !url.password;
  } catch { return false; }
}

function metadataObject(value: unknown): Readonly<Record<string, unknown>> | undefined {
  if (value === undefined) return {};
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  if (JSON.stringify(value).length > 32768) return undefined;
  return value as Record<string, unknown>;
}

export function readToolkitTestAuthorization(raw: unknown, toolkitId: string | number | undefined): ToolkitTestAuthorization | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const value = raw as Record<string, unknown>;
  if (!matchesToolkit(value['toolkit_id'], toolkitId)) return undefined;
  if (typeof value['toolkit_name'] !== 'string' || typeof value['toolkit_type'] !== 'string' || !value['toolkit_type']) return undefined;
  if (!validResource(value['server_url'])) return undefined;
  const metadata = metadataObject(value['resource_metadata']);
  if (metadata === undefined) return undefined;
  return {
    toolkit_id: Number(value['toolkit_id']), toolkit_name: value['toolkit_name'],
    toolkit_type: value['toolkit_type'], server_url: value['server_url'], resource_metadata: metadata,
    ...(typeof value['resource_metadata_url'] === 'string' ? { resource_metadata_url: value['resource_metadata_url'] } : {}),
  };
}
