import { useEffect, useRef, useState } from 'react';
import { Alert, Platform, type AppStateStatus } from 'react-native';

import { downloadAttachment, downloadErrorText } from '@/api/attachments/download';
import { previewFile, viewerFor, writeTempFile, type TempFile } from '@/features/chat/attachments/opener';
import type { ChatAttachment } from '@/features/chat/messageModel';
import { appActivity } from '@/lib/appActivity';

interface AppStateLike {
  addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void };
}

export interface OpenerDeps {
  download?: typeof downloadAttachment;
  write?: typeof writeTempFile;
  preview?: typeof previewFile;
  alert?: (message: string) => void;
  /** The app's activity (a locked app counts as background). */
  appState?: AppStateLike;
  platform?: typeof Platform.OS;
}

/**
 * Opens a message's file: downloads it (guarded fetch), writes it to a
 * temporary file, and shows it in the app's image viewer (raster images) or
 * the system preview (everything else). One open file at a time; its
 * temporary copy is deleted when the viewer closes, at the next open, when
 * the thread goes, and when the app leaves the foreground (or is locked).
 *
 * The system preview gives no "closed" signal (File.preview resolves once it
 * is presented). On iOS Quick Look runs inside the app, so leaving the
 * foreground ends it and the copy goes then. On Android the file is handed
 * to another app, which takes the foreground and may still be reading it,
 * so the copy goes when the user comes back instead.
 */
export function useAttachmentOpener(projectId: number | null, conversationUuid: string | null, deps: OpenerDeps = {}) {
  const [opening, setOpening] = useState<string | null>(null);
  const [viewer, setViewer] = useState<{ uri: string; name: string } | null>(null);
  const current = useRef<{ temp: TempFile; shown: 'image' | 'preview'; left: boolean } | null>(null);

  const release = () => {
    current.current?.temp.delete();
    current.current = null;
  };
  useEffect(() => release, []);

  const appState = deps.appState ?? appActivity;
  const external = (deps.platform ?? Platform.OS) === 'android';
  useEffect(() => {
    const sub = appState.addEventListener('change', (state) => {
      const open = current.current;
      if (open === null) return;
      if (state === 'background') {
        if (open.shown === 'image') {
          setViewer(null);
          release();
        } else if (external) {
          open.left = true;
        } else {
          release();
        }
      } else if (state === 'active' && open.shown === 'preview' && open.left) {
        release();
      }
    });
    return () => sub.remove();
  }, [appState, external]);

  const open = async (attachment: ChatAttachment) => {
    const conversation = attachment.conversationUuid ?? conversationUuid;
    if (projectId === null || conversation === null || opening !== null) return;
    release();
    setOpening(attachment.key);
    try {
      const { bytes } = await (deps.download ?? downloadAttachment)(projectId, conversation, attachment.storedName);
      const temp = (deps.write ?? writeTempFile)(bytes, attachment.storedName);
      const shown = viewerFor(bytes) === 'image' ? 'image' : 'preview';
      current.current = { temp, shown, left: false };
      if (shown === 'image') setViewer({ uri: temp.uri, name: attachment.name });
      else await (deps.preview ?? previewFile)(temp.uri);
    } catch (error) {
      release();
      (deps.alert ?? ((message: string) => Alert.alert(attachment.name, message)))(downloadErrorText(error));
    } finally {
      setOpening(null);
    }
  };

  const closeViewer = () => {
    setViewer(null);
    release();
  };

  return { opening, viewer, open, closeViewer };
}
