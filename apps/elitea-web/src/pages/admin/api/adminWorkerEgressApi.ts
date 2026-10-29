/**
 * REST client for the worker egress hosts view (ADR-0044 decision 2, #1490).
 * The operation is `listWorkerEgressHosts` in `v2.yaml`, so the call comes
 * from orval; this module holds the envelope peel and the query key.
 */
import { useQuery } from '@tanstack/react-query';

import { listWorkerEgressHosts } from '@/shared/api/generated/admin/admin';
import type { WorkerEgressHostsResponse } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

const KEY = ['admin', 'worker-egress-hosts'] as const;

/** Reads the hosts body out of the envelope or the bare body. */
export function workerEgressHosts(response: unknown): WorkerEgressHostsResponse {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null && Array.isArray((body as { reached?: unknown }).reached)) {
    return body as WorkerEgressHostsResponse;
  }
  throw new Error('worker egress hosts: the server answered with an unrecognised body');
}

/** Hosts reached and refused by the worker over the last `days` days. */
export function useWorkerEgressHosts(days: number) {
  return useQuery({
    queryKey: [...KEY, days],
    queryFn: async () => workerEgressHosts(await listWorkerEgressHosts({ days, source: 'all' })),
  });
}

/**
 * The allowlist entries that would admit every reached host: one `host` per
 * distinct name, or `host:port` when that host was only reached on ports
 * other than the scheme defaults. Sorted, deduplicated.
 */
export function allowlistFromReached(reached: readonly { host: string; port: number }[]): string[] {
  const ports = new Map<string, Set<number>>();
  for (const row of reached) {
    const host = row.host.toLowerCase();
    if (host === '') continue;
    const seen = ports.get(host) ?? new Set<number>();
    seen.add(row.port);
    ports.set(host, seen);
  }
  const entries: string[] = [];
  for (const [host, seen] of ports) {
    const nonDefault = [...seen].filter((port) => port !== 0 && port !== 80 && port !== 443);
    if (nonDefault.length === seen.size) {
      for (const port of nonDefault.sort((a, b) => a - b)) entries.push(`${host}:${port}`);
    } else {
      entries.push(host);
    }
  }
  return entries.sort();
}
