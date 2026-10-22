/**
 * Artifact domain type — a file stored in a bucket. The wire shapes below
 * mirror `ListObjectsResponse` / `objectSummary`
 * (services/elitea-main/internal/api/v2/artifacts/objects.go:39-46,
 * 250-254), the response of
 * `GET /api/v2/artifacts/objects/{projectID}/{bucket}`.
 *
 * They previously described the LEGACY runtime plugin's `S3ObjectListResponse`
 * (`{name, contents:[{key,size,lastModified}]}`), an envelope elitea-main
 * never served — see issue #138.
 *
 * There is no dedicated `Artifact` schema server-side — an artifact IS an
 * object summary; `bucket` is not carried per-item and is attached by
 * `lib/normalise.ts` from the bucket the list was requested for.
 */
export interface Artifact {
  readonly key: string;
  readonly size: number;
  /** ISO 8601 date-time. */
  readonly lastModified: string;
  readonly bucket: string;
}

/** One `ListObjectsResponse.objects[]` entry (objects.go:39-46). */
export interface ArtifactWireEntry {
  readonly key: string;
  readonly size_bytes: number;
  readonly media_type: string;
  readonly etag: string;
  readonly modified_at: string;
}

/** `ListObjectsResponse` (objects.go:250) — the un-normalised response envelope. */
export interface ArtifactListWire {
  readonly objects: readonly ArtifactWireEntry[];
  /** Keys collapsed by a `delimiter`; always empty for the flat list the UI requests. */
  readonly common_prefixes: readonly string[];
  readonly next_cursor?: string;
}
