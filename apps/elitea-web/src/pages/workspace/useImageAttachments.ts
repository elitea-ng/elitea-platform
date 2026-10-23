/**
 * The images attached to the composer's next message (#1229): pasted from the
 * clipboard, or dropped on the window as files (the shell calls
 * `attachFiles` with the paths the host reported). Each goes to the host,
 * which checks, shrinks and holds it in memory and answers a token and a
 * thumbnail; Send carries the tokens (`picks`).
 *
 * `enabled` is false for a thread that runs in the cloud (its turns take no
 * local images): nothing is offered, and a paste is left to the input.
 */
import { useCallback, useEffect, useRef, useState, type ClipboardEvent } from 'react';

import { describeWorkspaceError } from '@/features/workspace';
import { MAX_TURN_IMAGES, type AttachedImage, type TurnImagePick } from '@/shared/desktop/turnStart';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { SentImages } from './useSendPrompt';

export interface ImageAttachments {
  images: readonly AttachedImage[];
  /** Why the last image could not be attached; `null` once another is attached or it is dismissed. */
  error: string | null;
  /** What Send carries; `undefined` without images. */
  sent: () => SentImages | undefined;
  /** Take one off (the host forgets it). */
  remove: (index: number) => void;
  /** The message was sent: forget them here (the host used them up). */
  clear: () => void;
  /** The input's paste handler: takes image files off the clipboard, leaves text to the input. */
  onPaste: (event: ClipboardEvent<HTMLElement>) => void;
}

/** A file's bytes as base64, without the `data:` prefix. */
function base64Of(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const url = typeof reader.result === 'string' ? reader.result : '';
      resolve(url.slice(url.indexOf(',') + 1));
    };
    reader.onerror = () => reject(reader.error ?? new Error('read failed'));
    reader.readAsDataURL(file);
  });
}

export function useImageAttachments(ipc: WorkspaceIpc, workspaceId: string, enabled: boolean): ImageAttachments {
  const [entries, setEntries] = useState<{ image: AttachedImage; source: TurnImagePick['source'] }[]>([]);
  const [error, setError] = useState<string | null>(null);
  // How many are attached or on their way: the limit holds across quick pastes.
  const held = useRef(0);

  const adopt = useCallback(
    (source: TurnImagePick['source'], attach: () => Promise<AttachedImage>): void => {
      if (held.current >= MAX_TURN_IMAGES) return;
      held.current += 1;
      attach().then(
        (image) => {
          setError(null);
          setEntries((current) => [...current, { image, source }]);
        },
        (failure: unknown) => {
          held.current -= 1;
          setError(describeWorkspaceError(failure));
        },
      );
    },
    [],
  );

  const attachFiles = useCallback(
    (paths: string[]): void => {
      for (const path of paths) adopt('file', () => ipc.attachFile(workspaceId, path));
    },
    [adopt, ipc, workspaceId],
  );

  // The shell's file drop reaches the composer on screen through the session.
  useEffect(() => {
    if (!enabled) return undefined;
    useDesktopLayout.getState().extendSession({ attachFiles });
    return () => useDesktopLayout.getState().extendSession({ attachFiles: undefined });
  }, [enabled, attachFiles]);

  const onPaste = (event: ClipboardEvent<HTMLElement>): void => {
    if (!enabled) return;
    const files = Array.from(event.clipboardData.files).filter((file) => file.type.startsWith('image/'));
    if (files.length === 0) return;
    // An image on the clipboard is the paste; any text beside it is the image's caption in some apps, and is dropped.
    event.preventDefault();
    for (const file of files) {
      adopt('clipboard', async () => ipc.attachBytes(workspaceId, file.type, await base64Of(file), file.name === '' ? undefined : file.name));
    }
  };

  const remove = (index: number): void => {
    const entry = entries[index];
    if (entry === undefined) return;
    held.current -= 1;
    setEntries((current) => current.filter((_, at) => at !== index));
    void ipc.removeAttachment(workspaceId, entry.image.token).catch(() => undefined);
  };

  return {
    images: entries.map((entry) => entry.image),
    error,
    sent: () =>
      entries.length === 0
        ? undefined
        : { picks: entries.map((entry) => ({ source: entry.source, token: entry.image.token })), shown: entries.map(({ image }) => ({ name: image.name, thumbnail: image.thumbnail })) },
    remove,
    clear: () => {
      held.current = 0;
      setEntries([]);
      setError(null);
    },
    onPaste,
  };
}
