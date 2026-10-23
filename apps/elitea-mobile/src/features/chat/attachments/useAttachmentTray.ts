import { File } from 'expo-file-system';
import { useEffect, useState, useSyncExternalStore } from 'react';

import { uploadAttachment, uploadErrorText } from '@/api/attachments/upload';
import { PrepareError, readyFile } from '@/features/chat/attachments/imagePipeline';
import { discardPickerCopy } from '@/features/chat/attachments/pickers';
import type { AttachmentLimits } from '@/features/chat/attachments/prepare';
import { AttachmentTray } from '@/features/chat/attachments/tray';

function discard(uri: string) {
  try {
    const file = new File(uri);
    if (file.exists) file.delete();
  } catch {
    // swept when the workspace is put aside or ended (tempWipe.ts), and at the next start
  }
}

function exists(uri: string): boolean {
  try {
    return new File(uri).exists;
  } catch {
    return false;
  }
}

/** The composer's attachment tray for one conversation (cleared when the thread goes). */
export function useAttachmentTray(projectId: number | null, conversationUuid: string | null, limits: AttachmentLimits) {
  const [tray] = useState(
    () =>
      new AttachmentTray({
        limits,
        prepare: (file, { fitInline, limits: current }) => readyFile(file, current, { fitInline }),
        upload: (ready, onProgress, target, current, signal) =>
          uploadAttachment(
            target.projectId,
            target.conversationUuid,
            { uri: ready.uri, uploadName: ready.uploadName, size: ready.size, mimeType: ready.mimeType },
            current.chunkBytes,
            { onProgress, signal },
          ),
        discard,
        discardPicked: discardPickerCopy,
        exists,
        describe: (error, name) => (error instanceof PrepareError ? error.userMessage : uploadErrorText(error, name)),
      }),
  );
  // The workspace's limits may arrive (or change) after the thread opened.
  useEffect(() => tray.setLimits(limits), [tray, limits]);
  // The conversation is known only once its read arrives.
  useEffect(() => tray.setTarget(projectId, conversationUuid), [tray, projectId, conversationUuid]);
  useEffect(() => () => tray.clear(), [tray]);
  const items = useSyncExternalStore(tray.subscribe, tray.getSnapshot, tray.getSnapshot);
  return { tray, items };
}
