import { randomUUID } from 'expo-crypto';
import { Directory, File, Paths } from 'expo-file-system';
import { shareAsync } from 'expo-sharing';
import { Platform } from 'react-native';

import { ExportError, fetchTranscriptMarkdown } from '@/api/queries/exportConversation';
import { log } from '@/lib/log';
import { getPolicy } from '@/security/policy';
import { getSession } from '@/state/session';

/**
 * Export a transcript to the share sheet (client contract 1.3, gated by
 * `allow_share_out`): the Markdown is written to a temporary file in its own
 * random folder under the caches directory and handed to the system share
 * sheet as a FILE through expo-sharing (react-native's Share sends Android
 * only `title` and `message` and drops `url`, so an Android export shared
 * nothing).
 *
 * When the file is deleted differs by platform. iOS: as soon as the sheet
 * closes, whatever the user chose (the activity has taken its copy by then).
 * Android: the share promise settles when the chooser hands off, and the
 * receiving app may read the content URI later (a mail draft attaches it on
 * send), so the file stays until the sweep before the next export, or the
 * session's cache wipe. A file left behind on iOS (the app killed while the
 * sheet was open) is swept the same way.
 */
export const EXPORT_TEMP_DIR = 'export-tmp';

export interface ExportDeps {
  fetchTranscript?: typeof fetchTranscriptMarkdown;
  /** Writes the text and returns its file uri plus how to delete it. */
  writeTemp?: (text: string, fileName: string) => { uri: string; delete: () => void };
  share?: (uri: string, fileName: string) => Promise<unknown>;
  sweep?: () => void;
  /** Defaults to Platform.OS. */
  platform?: string;
}

export const EXPORT_MIME_TYPE = 'text/markdown';
const EXPORT_UTI = 'net.daringfireball.markdown';

function shareFile(uri: string, fileName: string): Promise<void> {
  return shareAsync(uri, { mimeType: EXPORT_MIME_TYPE, UTI: EXPORT_UTI, dialogTitle: fileName });
}

function writeTempText(text: string, fileName: string): { uri: string; delete: () => void } {
  const folder = new Directory(Paths.cache, EXPORT_TEMP_DIR, randomUUID());
  folder.create({ intermediates: true });
  const file = new File(folder, fileName);
  file.write(text);
  return {
    uri: file.uri,
    delete: () => {
      try {
        if (folder.exists) folder.delete();
      } catch (error) {
        log.info('export file not deleted; swept later', { error });
      }
    },
  };
}

export function sweepExportTemp(): void {
  try {
    const dir = new Directory(Paths.cache, EXPORT_TEMP_DIR);
    if (dir.exists) dir.delete();
  } catch {
    // best effort
  }
}

/**
 * The screen checked `allow_share_out` when it rendered Export; the download
 * can take a while, and a policy tightening or a workspace switch can land in
 * between. Checked again right before anything is written or shared.
 */
function assertStillShareable(started: { serverOrigin: string | null; workspaceId: string | null }): void {
  if (!getPolicy().allow_share_out) throw new ExportError('not_allowed');
  const now = getSession();
  if (now.ending || now.switching || now.serverOrigin !== started.serverOrigin || now.workspaceId !== started.workspaceId) {
    throw new ExportError('cancelled');
  }
}

/** Fetches, shares and (on iOS) deletes. Throws ExportError or the share sheet's error. */
export async function shareTranscript(projectId: number, conversationId: string, deps: ExportDeps = {}): Promise<void> {
  const { serverOrigin, workspaceId } = getSession();
  (deps.sweep ?? sweepExportTemp)();
  const { markdown, fileName } = await (deps.fetchTranscript ?? fetchTranscriptMarkdown)(projectId, conversationId);
  assertStillShareable({ serverOrigin, workspaceId });
  const temp = (deps.writeTemp ?? writeTempText)(markdown, fileName);
  const deleteWhenClosed = (deps.platform ?? Platform.OS) === 'ios';
  try {
    await (deps.share ?? shareFile)(temp.uri, fileName);
  } finally {
    if (deleteWhenClosed) temp.delete();
  }
}
