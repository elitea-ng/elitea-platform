/**
 * REST client for the entitlement export (ADR-0044 decision 5, #1501): who
 * holds which role on which project and agent, and which governance
 * capabilities each role carries. `exportEntitlements` comes from orval; this
 * module saves the CSV the server streams.
 */
import { exportEntitlements } from '@/shared/api/generated/admin/admin';
import { filenameFromContentDisposition, triggerBlobDownload } from '@/shared/lib/download';

/** The permission the export route requires (administration mode). */
export const ENTITLEMENTS_EXPORT_PERMISSION = 'entitlements.export';

/**
 * Downloads every entitlement row as CSV (`format=csv`), under the server's
 * filename when it names one. Rejects when the server refuses (eliteaFetch
 * throws on a non-2xx) or answers with no text, so the caller can say so.
 */
export async function downloadEntitlementsCsv(): Promise<void> {
  const response = (await exportEntitlements({ format: 'csv' })) as {
    readonly data: unknown;
    readonly headers?: Headers;
  };
  if (typeof response.data !== 'string') {
    throw new Error('entitlements export: the server answered with no CSV');
  }
  const filename = filenameFromContentDisposition(
    response.headers?.get('content-disposition') ?? null,
    'entitlements.csv',
  );
  triggerBlobDownload(new Blob([response.data], { type: 'text/csv;charset=utf-8' }), filename);
}
