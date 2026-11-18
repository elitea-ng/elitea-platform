// @ts-nocheck
import { useCallback, useEffect, useRef, useState } from 'react';

import { ChatParticipantType } from '../../model/constants';

import { useSelectedProjectId } from '../../api/useSelectedProjectId';
import { useFetchParticipantDetails } from './useFetchParticipantDetails';

// ---------------------------------------------------------------------------
// FetchOptions — shared across the detail layer
// ---------------------------------------------------------------------------

interface FetchOptions {
  forceRefetch?: boolean;
}

// ---------------------------------------------------------------------------
// Helper: fetch version details (complexity ≤ 4)
// ---------------------------------------------------------------------------

async function fetchAndMergeVersionDetails(
  versionId: string | undefined,
  details: Record<string, unknown>,
  fetchOriginalVersionDetails: (
    type: ChatParticipantType,
    id: string,
    versionId: string,
    projectId: string,
    versionName: string,
  ) => Promise<Record<string, unknown>>,
  entityName: ChatParticipantType,
  entityMeta: Record<string, unknown> | undefined,
  entityProjectId: string,
): Promise<Record<string, unknown>> {
  if (!versionId) return details.version_details || {};
  // A participant can carry the id as a number, the version list as strings (or the reverse); a strict
  // comparison then fell back to the DEFAULT version and showed its starters and welcome message.
  const wanted = String(versionId);
  if (String((details.version_details as Record<string, unknown>)?.id) === wanted) return details.version_details || {};

  const versions = (details.versions as Record<string, unknown>[]) || [];
  const versionExists = versions.some((v) => String(v.id) === wanted);
  if (!versionExists) return details.version_details || {};

  const versionName = versions.find((v) => String(v.id) === wanted)?.name || '';
  return fetchOriginalVersionDetails(
    entityName,
    String(entityMeta?.id),
    wanted,
    entityProjectId,
    versionName,
  );
}

// ---------------------------------------------------------------------------
// useActiveParticipantDetails — full implementation
// ---------------------------------------------------------------------------

const EMPTY_DETAILS: Record<string, unknown> = {};

/** Who the details belong to: type, entity id and owning project. The version is not part of it: a version switch keeps the same participant. */
function participantKey(participant: Record<string, unknown> | null | undefined): string | undefined {
  if (!participant) return undefined;
  const meta = (participant['entity_meta'] ?? {}) as Record<string, unknown>;
  return `${String(participant['entity_name'])}:${String(meta['id'])}:${String(meta['project_id'])}`;
}

interface LoadedDetails {
  readonly key: string | undefined;
  readonly details: Record<string, unknown>;
  readonly failed: boolean;
}
const NOTHING_LOADED: LoadedDetails = { key: undefined, details: EMPTY_DETAILS, failed: false };

export interface UseActiveParticipantDetailsResult {
  /** Details of THIS participant only; empty while they load, after a failed read, and for a participant whose read has not answered. */
  activeParticipantDetails: Record<string, unknown>;
  isLoadingDetails: boolean;
  /** The read for this participant failed; consumers that act on the details (the version-missing guard) must not treat empty as "no versions". */
  isDetailsFailed: boolean;
  refetchParticipantDetails: (options?: FetchOptions) => Promise<void>;
}

/**
 * Hook that fetches and caches detail data for the active participant.
 * Ported from `useActiveParticipantDetails.hooks.js`.
 *
 * Delegates all network fetching to `useFetchParticipantDetails`, which
 * implements the full data-fetching logic:
 * - Applications/pipelines: generated `useGetApplication`/`useGetPublicApplication`
 * - Version details: generated `useGetApplicationVersionDetail`
 * - Toolkits: client-side list lookup (no GET-single endpoint exists)
 */
export function useActiveParticipantDetails(
  props: { activeParticipant?: Record<string, unknown> | null; skip?: boolean },
): UseActiveParticipantDetailsResult {
  const { activeParticipant, skip } = props;
  const projectId = useSelectedProjectId();
  const { fetchOriginalDetails, fetchOriginalVersionDetails } = useFetchParticipantDetails();

  // Keyed by participant: details of the PREVIOUS participant would otherwise stay
  // visible until the next read answers, and the chat's version-missing guard
  // wrote that agent's base version onto the new participant (issue 2053).
  const [loaded, setLoaded] = useState<LoadedDetails>(NOTHING_LOADED);
  const currentKey = participantKey(activeParticipant);
  const owned = currentKey !== undefined && loaded.key === currentKey;
  const [isLoadingDetails, setIsLoadingDetails] = useState(false);
  // Only the latest request may write. The target can change while a read is
  // in flight (a chat with nothing picked resolves to one agent, then to the
  // agent its history last addressed once that loads), and a slower answer
  // for the earlier target used to land last and stick: the attachments gate
  // and the grantable tools then read the wrong agent's version.
  const latestRequest = useRef(0);

  const fetchDetails = useCallback(
    async (options?: FetchOptions) => {
      if (!activeParticipant) return;

      latestRequest.current += 1;
      const requestKey = participantKey(activeParticipant);
      const request = latestRequest.current;
      setIsLoadingDetails(true);
      try {
        const entityName = activeParticipant.entity_name as ChatParticipantType;
        const entityMeta = activeParticipant.entity_meta as Record<string, unknown>;
        const entitySettings = activeParticipant.entity_settings as Record<string, unknown>;
        const entityProjectId = (entityMeta?.project_id as string) || projectId;

        const details = await fetchOriginalDetails(entityName, String(entityMeta?.id), entityProjectId, options);

        const versionDetails = await fetchAndMergeVersionDetails(
          entitySettings?.version_id as string | undefined,
          details,
          fetchOriginalVersionDetails,
          entityName,
          entityMeta,
          entityProjectId,
        );

        if (request !== latestRequest.current) return;
        setLoaded({ key: requestKey, details: { ...details, version_details: versionDetails }, failed: false });
      } catch (error) {
        if (request === latestRequest.current) setLoaded({ key: requestKey, details: EMPTY_DETAILS, failed: true });
        throw error;
      } finally {
        if (request === latestRequest.current) setIsLoadingDetails(false);
      }
    },
    [activeParticipant, fetchOriginalDetails, fetchOriginalVersionDetails, projectId],
  );

  useEffect(() => {
    if (activeParticipant && !skip) {
      fetchDetails().catch(() => {});
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- activeParticipant id + project + name + version_id drive re-fetch
  }, [
    activeParticipant?.entity_meta?.id,
    activeParticipant?.entity_meta?.project_id,
    activeParticipant?.entity_name,
    activeParticipant?.entity_settings?.version_id,
  ]);

  useEffect(() => {
    if (!activeParticipant) {
      setLoaded(NOTHING_LOADED);
    }
  }, [activeParticipant]);

  const refetchParticipantDetails = useCallback(
    async (options?: FetchOptions) => {
      await fetchDetails({ forceRefetch: true, ...options });
    },
    [fetchDetails],
  );

  return {
    activeParticipantDetails: owned ? loaded.details : EMPTY_DETAILS,
    isLoadingDetails,
    isDetailsFailed: owned && loaded.failed,
    refetchParticipantDetails,
  };
}
