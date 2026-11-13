// @ts-nocheck
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

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

function isIdLike(value: unknown): value is string | number {
  return typeof value === 'string' || typeof value === 'number';
}

export interface UseActiveParticipantDetailsResult {
  activeParticipantDetails: Record<string, unknown>;
  isLoadingDetails: boolean;
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

  const [activeParticipantDetails, setActiveParticipantDetails] = useState<Record<string, unknown>>({});
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
        setActiveParticipantDetails({
          ...details,
          version_details: versionDetails,
        });
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
      setActiveParticipantDetails({});
    }
  }, [activeParticipant]);

  const refetchParticipantDetails = useCallback(
    async (options?: FetchOptions) => {
      await fetchDetails({ forceRefetch: true, ...options });
    },
    [fetchDetails],
  );

  // Details of the PREVIOUS participant stay in state until the new read lands.
  // Handed out as the new participant's, their version list made the chat's
  // "version missing" guard pick that agent's base version and write it onto
  // the new participant (404 "version not found" on the second agent added,
  // issue 2053). Details that name another entity are not this one's.
  const ownedId = activeParticipant?.entity_meta?.id;
  const ownedDetails = useMemo(() => {
    const detailsId = activeParticipantDetails['id'];
    if (!isIdLike(ownedId) || !isIdLike(detailsId)) return activeParticipantDetails;
    return String(detailsId) === String(ownedId) ? activeParticipantDetails : EMPTY_DETAILS;
  }, [activeParticipantDetails, ownedId]);

  return {
    activeParticipantDetails: ownedDetails,
    isLoadingDetails,
    refetchParticipantDetails,
  };
}
