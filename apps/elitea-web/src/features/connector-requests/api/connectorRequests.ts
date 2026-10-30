/**
 * REST client for connector requests, project side (ADR-0044 decision 1,
 * #1501). In the `approved_only` catalogue mode a toolkit type, a pre-built
 * MCP server or a custom MCP server URL is usable only after a connector
 * approver approves a request naming the project. The operations come from
 * orval (`createConnectorRequest`, `listConnectorRequests`,
 * `withdrawConnectorRequest`); this module holds the refusal reader, the
 * envelope peel and the query keys.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import type {
  ConnectorDeclaredTool,
  ConnectorRequest,
  ConnectorRequestCreate,
  ConnectorTargetKind,
  ConnectorToolCategory,
} from '@/shared/api/generated/model';
import { createConnectorRequest, listConnectorRequests } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/** The connector a refused write names: what a request would approve. */
export interface ConnectorTarget {
  readonly targetKind: ConnectorTargetKind;
  readonly target: string;
}

const TARGET_KINDS: readonly string[] = ['toolkit_type', 'prebuilt_mcp', 'custom_mcp'];

function errorBody(error: unknown): Record<string, unknown> | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const body = error.failure.body;
  return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : undefined;
}

/**
 * The connector an error refused (403 `connector_not_approved`), or undefined
 * when the error is anything else. A refusal without a target (an MCP URL
 * that is not valid) is undefined too: there is nothing a request could
 * approve.
 */
export function readConnectorRefusal(error: unknown): ConnectorTarget | undefined {
  const body = errorBody(error);
  if (body === undefined) return undefined;
  const code = body['code'] ?? body['error'];
  const kind = body['target_kind'];
  const target = body['target'];
  if (code !== 'connector_not_approved' || typeof kind !== 'string' || !TARGET_KINDS.includes(kind)) return undefined;
  if (typeof target !== 'string' || target === '') return undefined;
  return { targetKind: kind as ConnectorTargetKind, target };
}

/** The server's own sentence for a refused request, or a generic one. */
export function describeConnectorRequestError(error: unknown): string {
  const message = errorBody(error)?.['message'];
  if (typeof message === 'string' && message !== '') return message;
  return t('features.connectorRequests.error.generic', 'The connector request could not be sent. Try again.');
}

/** The ADR-0041 tool categories, in the order the form offers them. */
export const TOOL_CATEGORIES: readonly ConnectorToolCategory[] = [
  'read',
  'write',
  'delete',
  'external_send',
  'payment',
  'code_execution',
];

/** Splits a comma or newline separated list, dropping blanks and repeats. */
export function splitList(value: string): string[] {
  return [...new Set(value.split(/[\n,]/).map((item) => item.trim()).filter((item) => item !== ''))];
}

const KEY = ['connector-requests'] as const;

function requestRows(response: unknown): ConnectorRequest[] {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null) {
    const rows = (body as { rows?: unknown }).rows;
    if (Array.isArray(rows)) return rows as ConnectorRequest[];
  }
  throw new Error('connector requests: the server answered with an unrecognised body');
}

/** The project's requests, newest first. */
export function useProjectConnectorRequests(projectId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: [...KEY, projectId],
    queryFn: async () => requestRows(await listConnectorRequests(projectId ?? '')),
    enabled: enabled && projectId !== undefined && projectId !== '',
  });
}

export interface ConnectorRequestInput {
  readonly target: ConnectorTarget;
  readonly tools: readonly ConnectorDeclaredTool[];
  readonly egressHosts: readonly string[];
  readonly dataClasses: readonly string[];
  readonly justification: string;
  readonly platformWide: boolean;
}

/** Sends a request straight to the approvers (`submit: true`). */
export function useCreateConnectorRequest(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ConnectorRequestInput): Promise<ConnectorRequest> => {
      const body: ConnectorRequestCreate = {
        target_kind: input.target.targetKind,
        target: input.target.target,
        tools: input.tools.map((tool) => ({ ...tool })),
        egress_hosts: [...input.egressHosts],
        data_classes: [...input.dataClasses],
        justification: input.justification,
        platform_wide: input.platformWide,
        submit: true,
      };
      return unwrapBody(await createConnectorRequest(projectId ?? '', body)) as ConnectorRequest;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: KEY }),
  });
}
