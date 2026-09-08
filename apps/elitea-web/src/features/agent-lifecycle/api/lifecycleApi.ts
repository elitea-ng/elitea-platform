import {
  exportApplication,
  forkAgent,
  importWizard,
  publishApplication,
  unpublishApplication,
  validateForPublish,
} from '@/shared/api/generated/applications/applications';
import type {
  ForkRequest,
  ImportWizardRequest,
  PublishRequest,
  PublishValidationResult,
} from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The lifecycle plane, as the generated client already serves it.
 *
 * Every route below was generated from `v2.yaml` months ago and had ZERO
 * callers — `src/shared/api/endpoints.manifest.json` recorded `"usedBy": []`
 * for publish, unpublish, publish_validate, fork and import_wizard alike. The
 * server half is real and permission-gated; the app simply never called it.
 * Nothing here needs a spec change, and this file adds no new URL.
 *
 * What it does add is the unwrapping. `eliteaFetch` resolves the ENVELOPE
 * `{data, status, headers}` and the generated `Promise<T>` annotation is a
 * cast over it, so reading a response as the body gives undefined fields on a
 * perfectly good 200. Every function here reads through `.data`.
 */

interface Envelope<T> {
  readonly data: T;
  readonly status: number;
}

/**
 * The FAIL body of `validateForPublish`, which arrives as an error rather than
 * a response. An absent body becomes an empty result, which
 * `normalizePublishValidation` reads as a FAIL with no findings — not as a
 * pass.
 */
function body422(payload: unknown): PublishValidationResult {
  if (typeof payload !== 'object' || payload === null) return {};
  return payload;
}

function body<T>(envelope: unknown): T {
  return (envelope as Envelope<T>).data;
}

/** The export document, which is also the import and fork document. */
export interface EntityExportDocument {
  readonly ok?: boolean;
  readonly applications?: readonly unknown[];
  readonly toolkits?: readonly unknown[];
  readonly skills?: readonly unknown[];
}

export interface PublishOutcome {
  readonly publicAgentId?: string;
  readonly publicVersionId?: string;
  readonly versionName?: string;
  /**
   * The catalogue rows the publish created, present only when the author's
   * project is not itself the public project. Omitted rather than zeroed by
   * the server (`internal/api/v2/eliteacore/handler.go`), so `undefined` here
   * means "the clone already IS the catalogue row", never "row 0".
   */
  readonly catalogAgentId?: string;
  readonly catalogVersionId?: string;
}

/**
 * Runs the pre-publish validation.
 *
 * A **422 is a result, not a failure**: `validateForPublish` answers 200 on
 * PASS/WARN and 422 on FAIL with the identical body. `eliteaFetch` throws an
 * `EliteaApiError` on any non-2xx, so the FAIL body has to be recovered from
 * the thrown error — reporting it as a transport error would show the author
 * "validation could not run" for the one case where it ran and said no.
 */
export async function runPublishValidation(
  projectId: string,
  versionId: number,
  versionName: string,
  category?: string,
): Promise<PublishValidationResult> {
  try {
    const envelope = await validateForPublish(projectId, versionId, {
      version_name: versionName,
      ...(category === undefined || category === '' ? {} : { category }),
    });
    return body<PublishValidationResult>(envelope);
  } catch (cause) {
    if (cause instanceof EliteaApiError && cause.failure.kind === 'http' && cause.failure.status === 422) {
      return body422(cause.failure.body);
    }
    throw cause;
  }
}

export async function publishVersion(
  projectId: string,
  versionId: number,
  request: PublishRequest,
): Promise<PublishOutcome> {
  const envelope = await publishApplication(projectId, versionId, request);
  const raw = body<Record<string, unknown>>(envelope);
  return {
    ...(typeof raw['public_agent_id'] === 'string' ? { publicAgentId: raw['public_agent_id'] } : {}),
    ...(typeof raw['public_version_id'] === 'string' ? { publicVersionId: raw['public_version_id'] } : {}),
    ...(typeof raw['version_name'] === 'string' ? { versionName: raw['version_name'] } : {}),
    ...(typeof raw['catalog_agent_id'] === 'string' ? { catalogAgentId: raw['catalog_agent_id'] } : {}),
    ...(typeof raw['catalog_version_id'] === 'string' ? { catalogVersionId: raw['catalog_version_id'] } : {}),
  };
}

export async function unpublishVersion(projectId: string, versionId: number): Promise<void> {
  await unpublishApplication(projectId, versionId, {});
}

/**
 * Reads the entity as a FORK document.
 *
 * `?fork=true` is what makes the export usable as a fork input: it keeps only
 * the latest version and stamps `owner_id`, `original_exported` and the
 * `shared_*` origin the fork handler writes into `meta.parent_*`. The plain
 * export omits all four, so forking a plain export produces a copy that does
 * not know where it came from (`internal/api/v2/eliteacore/handler.go`).
 */
export async function fetchForkDocument(projectId: string, entityId: number): Promise<EntityExportDocument> {
  const envelope = await exportApplication(projectId, entityId, { fork: true });
  return body<EntityExportDocument>(envelope);
}

/** Reads the entity as a plain export document — the file Import round-trips. */
export async function fetchExportDocument(projectId: string, entityId: number): Promise<EntityExportDocument> {
  const envelope = await exportApplication(projectId, entityId);
  return body<EntityExportDocument>(envelope);
}

export interface ForkOutcome {
  /** 201 when everything forked, 207 when part of it did. */
  readonly status: number;
  readonly result: unknown;
  readonly errors: unknown;
}

export async function forkEntity(targetProjectId: string, document: EntityExportDocument): Promise<ForkOutcome> {
  const request = {
    applications: document.applications ?? [],
    ...(document.skills === undefined ? {} : { skills: document.skills }),
  } as ForkRequest;
  const envelope = (await forkAgent(targetProjectId, request)) as Envelope<Record<string, unknown>>;
  return {
    status: envelope.status,
    result: envelope.data['result'],
    errors: envelope.data['errors'],
  };
}

export interface ImportOutcome {
  readonly status: number;
  readonly result: unknown;
  readonly errors: unknown;
}

/**
 * Posts a whole export document to the import wizard.
 *
 * The document goes through unchanged, including its `toolkits` and `skills`
 * arrays. The generated `ImportWizardRequest` type describes only the two
 * shapes the SPEC lists (a bare entity array, or `{applications: [...]}`),
 * while the handler reads `toolkits` and `skills` from the same envelope
 * (`handler.go` ExportImportPost). Narrowing the body to the spec's two shapes
 * would silently drop the toolkits and skills half of every imported agent, so
 * the document is sent whole and the type is asserted at this one boundary.
 */
export async function importEntities(projectId: string, document: EntityExportDocument): Promise<ImportOutcome> {
  const envelope = (await importWizard(
    projectId,
    document as unknown as ImportWizardRequest,
  )) as Envelope<Record<string, unknown>>;
  return {
    status: envelope.status,
    result: envelope.data['result'],
    errors: envelope.data['errors'],
  };
}
