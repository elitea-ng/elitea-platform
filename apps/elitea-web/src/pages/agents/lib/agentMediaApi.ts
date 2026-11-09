/**
 * ADR-0050 decision 6 — the Avatar tab's two writes, over the generated
 * fetchers: one upload into the project's `agent-media` bucket, and one
 * generation through the LLM gateway. Neither touches the version; the tab
 * puts the answered url into the draft pack and the editor's Save writes it.
 *
 * The fetchers resolve the transport envelope `{data, status, headers}`;
 * each call here unwraps it once (#132).
 */
import { useMutation, type UseMutationResult } from '@tanstack/react-query';

import { generateAgentMedia, uploadAgentMedia } from '@/shared/api/generated/applications/applications';
import type { AgentMediaGenerateRequest, AgentMediaGenerateResponse, AgentMediaObject } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: applications.uploadAgentMedia */
export async function uploadAgentMediaFile(projectId: string, file: File): Promise<AgentMediaObject> {
  return bodyOf<AgentMediaObject>(await uploadAgentMedia(projectId, { file }));
}

/* manifest: applications.generateAgentMedia */
export async function generateAgentMediaFiles(projectId: string, request: AgentMediaGenerateRequest): Promise<readonly AgentMediaObject[]> {
  return bodyOf<AgentMediaGenerateResponse>(await generateAgentMedia(projectId, request)).media;
}

/** The server's reason for a refusal (`{error, code}`), else the generic message. */
export function agentMediaErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const body = error.failure.body as { readonly error?: unknown } | null | undefined;
    if (typeof body?.error === 'string' && body.error.trim() !== '') return body.error;
  }
  return fallback;
}

export function useUploadAgentMedia(projectId: string): UseMutationResult<AgentMediaObject, Error, File> {
  return useMutation({ mutationFn: (file) => uploadAgentMediaFile(projectId, file) });
}

export function useGenerateAgentMedia(
  projectId: string,
): UseMutationResult<readonly AgentMediaObject[], Error, AgentMediaGenerateRequest> {
  return useMutation({ mutationFn: (request) => generateAgentMediaFiles(projectId, request) });
}
