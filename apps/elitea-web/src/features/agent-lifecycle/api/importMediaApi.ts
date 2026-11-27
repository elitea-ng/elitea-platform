import { uploadAgentMedia } from '@/shared/api/generated/applications/applications';
import type { AgentMediaObject } from '@/shared/api/generated/model';

import type { UploadedMedia } from '../lib/importAvatars';

/**
 * One avatar file of an imported zip into the project's agent-media bucket
 * (issue 3137): the upload the editor's Avatar tab makes. The fetcher
 * resolves the `{data, status}` envelope, unwrapped once here.
 */
/* manifest: applications.uploadAgentMedia */
export async function uploadImportMedia(projectId: string, file: File): Promise<UploadedMedia> {
  const media = ((await uploadAgentMedia(projectId, { file })) as unknown as { readonly data: AgentMediaObject }).data;
  return { url: media.url, media_type: media.media_type };
}
