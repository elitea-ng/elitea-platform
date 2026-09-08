import { type ComponentProps, useCallback, useMemo, useState } from 'react';
import Alert from '@mui/material/Alert';

import { McpAuthModal, useGetRemoteMcpTools } from '@/features/mcps';
import { ToolkitForm } from '@/features/toolkits';

import type { EditToolDetail } from './toolkitFormTypes';

type Options = Pick<ComponentProps<typeof ToolkitForm>, 'onChangeToolDetail' | 'projectId'> & { editToolDetail: EditToolDetail | null };
type DiscoveryValues = NonNullable<Parameters<typeof useGetRemoteMcpTools>[0]['values']>;

function stringValue(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function discoveryValues(detail: EditToolDetail | null | undefined): DiscoveryValues {
  const settings = detail?.settings ?? {};
  const headers = settings['headers'];
  return {
    id: detail?.id === undefined ? undefined : String(detail.id),
    type: detail?.type,
    settings: {
      url: stringValue(settings['url']),
      client_id: stringValue(settings['client_id']),
      client_secret: stringValue(settings['client_secret']),
      scopes: Array.isArray(settings['scopes']) ? settings['scopes'].filter((v): v is string => typeof v === 'string') : stringValue(settings['scopes']),
      headers: headers && typeof headers === 'object' ? Object.fromEntries(Object.entries(headers).filter((entry): entry is [string, string] => typeof entry[1] === 'string')) : undefined,
      timeout: typeof settings['timeout'] === 'number' ? settings['timeout'] : undefined,
      ssl_verify: typeof settings['ssl_verify'] === 'boolean' ? settings['ssl_verify'] : undefined,
    },
  };
}

/** Compose MCP discovery at the page boundary, without sideways feature imports. */
export function useMcpDiscoverySlot({ editToolDetail, onChangeToolDetail, projectId }: Options) {
  const values = useMemo(() => discoveryValues(editToolDetail), [editToolDetail]);
  const key = `${projectId}:${values.id}:${values.type}:${values.settings?.url}`;
  const [preview, setPreview] = useState<{ key: string; tools: string[] }>();
  const [error, setError] = useState<string>();
  const onToolsFetched = useCallback((tools: readonly unknown[]) => {
    const names = tools.flatMap((tool) => {
      if (typeof tool === 'string') return [tool];
      return tool && typeof tool === 'object' && 'name' in tool && typeof tool.name === 'string' ? [tool.name] : [];
    });
    setPreview({ key, tools: names });
    setError(undefined);
    onChangeToolDetail?.((current) => {
      if (!current || names.length === 0) return current;
      const selected = current.settings?.['selected_tools'];
      if (Array.isArray(selected) && selected.length > 0) return current;
      return { ...current, settings: { ...current.settings, selected_tools: names } };
    });
  }, [key, onChangeToolDetail]);
  const discovery = useGetRemoteMcpTools({ values, projectId, onToolsFetched, onError: setError });
  const isMcp = values.type === 'mcp' || values.type?.startsWith('mcp_');
  if (!isMcp) return undefined;
  return {
    availableTools: preview?.key === key ? preview.tools : undefined,
    onLoadTools: discovery.fetchTools,
    isLoadingTools: discovery.isLoading,
    canLoadTools: projectId !== undefined && (values.type !== 'mcp' || Boolean(values.settings?.url)),
    mcpAuthModal: <>{error && <Alert severity="error">{error}</Alert>}<McpAuthModal {...discovery.getModalProps()} /></>,
  };
}
