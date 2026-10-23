/**
 * The images attached to the composer's next message (#1229): pasted from the
 * clipboard, or dropped on the window as files (the shell calls
 * `attachFiles` with the paths the host reported). Each goes to the host,
 * which checks, shrinks and holds it in memory and answers a token and a
 * thumbnail; Send carries the tokens (`picks`).
 *
 * `enabled` is false for a thread that runs in the cloud (its turns take no
 * local images): nothing is offered, and a paste is left to the input.
 *
 * The images belong to one workspace: when the composer moves to another, or
 * goes away, the ones not sent are let go (the host forgets their tokens), and
 * one still on its way is let go when it lands.
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
  // The tokens the host holds for this composer and not yet sent: let go when the workspace changes or the composer goes.
  const pending = useRef<string[]>([]);
  // Which workspace's attachments are current: an answer for an earlier one is let go, not shown.
  const generation = useRef(0);

  useEffect(() => {
    const ours = generation.current;
    return () => {
      generation.current = ours + 1;
      const tokens = pending.current;
      pending.current = [];
      held.current = 0;
      setEntries([]);
      setError(null);
      for (const token of tokens) void ipc.removeAttachment(workspaceId, token).catch(() => undefined);
    };
  }, [ipc, workspaceId]);

  const adopt = useCallback(
    (source: TurnImagePick['source'], attach: () => Promise<AttachedImage>, release: (token: string) => void): void => {
      if (held.current >= MAX_TURN_IMAGES) return;
      held.current += 1;
      const ours = generation.current;
      attach().then(
        (image) => {
          if (generation.current !== ours) {
            release(image.token);
            return;
          }
          pending.current = [...pending.current, image.token];
          setError(null);
          setEntries((current) => [...current, { image, source }]);
        },
        (failure: unknown) => {
          if (generation.current !== ours) return;
          held.current -= 1;
          setError(describeWorkspaceError(failure));
        },
      );
    },
    [],
  );

  const release = useCallback(
    (token: string): void => {
      void ipc.removeAttachment(workspaceId, token).catch(() => undefined);
    },
    [ipc, workspaceId],
  );

  const attachFiles = useCallback(
    (paths: string[]): void => {
      for (const path of paths) adopt('file', () => ipc.attachFile(workspaceId, path), release);
    },
    [adopt, ipc, workspaceId, release],
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
      adopt('clipboard', async () => ipc.attachBytes(workspaceId, file.type, await base64Of(file), file.name === '' ? undefined : file.name), release);
    }
  };

  const remove = (index: number): void => {
    const entry = entries[index];
    if (entry === undefined) return;
    held.current -= 1;
    pending.current = pending.current.filter((token) => token !== entry.image.token);
    setEntries((current) => current.filter((_, at) => at !== index));
    release(entry.image.token);
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
      // Sent: the turn used them up, nothing is left to let go.
      pending.current = [];
      setEntries([]);
      setError(null);
    },
    onPaste,
  };
}
