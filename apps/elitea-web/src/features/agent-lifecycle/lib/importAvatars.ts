/**
 * The avatar media of an imported zip (issue 3137), uploaded on confirm: each
 * file goes through the Avatar tab's own upload
 * (`POST /elitea_core/agent_media/prompt_lib/{project_id}`), and the url the
 * server answers becomes the slot of the version's `avatar_pack`. The import
 * body then carries urls, never bytes.
 */
import { AVATAR_PACK_SCHEMA } from '@elitea/client-core/conversation';

import type { EntityExportDocument } from '../api/lifecycleApi';
import type { PendingAvatar } from './importTypes';

export interface UploadedMedia {
  readonly url: string;
  readonly media_type?: string | undefined;
}

type UploadMedia = (file: File) => Promise<UploadedMedia>;

const MEDIA_TYPES: Readonly<Record<string, string>> = {
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  webp: 'image/webp',
  svg: 'image/svg+xml',
  mp4: 'video/mp4',
  webm: 'video/webm',
};

/** A `File` of the avatar; the type is read from the extension (the server sniffs the bytes anyway). */
function avatarFile(avatar: PendingAvatar): File {
  const name = avatar.path.split('/').pop() ?? avatar.slot;
  const extension = name.split('.').pop()?.toLowerCase() ?? '';
  return new File([avatar.bytes as BlobPart], name, { type: MEDIA_TYPES[extension] ?? 'application/octet-stream' });
}

type Rec = Record<string, unknown>;

function withPacks(entry: Rec, packs: ReadonlyMap<string, Rec>): Rec {
  if (!Array.isArray(entry['versions'])) return entry;
  return {
    ...entry,
    versions: (entry['versions'] as Rec[]).map((version) => {
      const slots = packs.get(String(version['import_version_uuid']));
      return slots === undefined ? version : { ...version, avatar_pack: { schema: AVATAR_PACK_SCHEMA, slots } };
    }),
  };
}

/** Uploads every pending avatar, one at a time, and answers the document with each version's pack filled in. */
export async function attachImportAvatars(
  document: EntityExportDocument,
  avatars: readonly PendingAvatar[],
  upload: UploadMedia,
): Promise<EntityExportDocument> {
  if (avatars.length === 0) return document;
  const packs = new Map<string, Rec>();
  for (const avatar of avatars) {
    const media = await upload(avatarFile(avatar));
    const slots = packs.get(avatar.versionKey) ?? {};
    slots[avatar.slot] = { url: media.url, ...(media.media_type === undefined ? {} : { media_type: media.media_type }) };
    packs.set(avatar.versionKey, slots);
  }
  return { ...document, applications: (document.applications ?? []).map((entry) => withPacks(entry as Rec, packs)) };
}
