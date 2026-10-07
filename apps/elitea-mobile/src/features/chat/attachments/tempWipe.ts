import { sweepUploadTemp } from '@/api/attachments/upload';
import { sweepDownloadTemp } from '@/features/chat/attachments/opener';
import { sweepPickerCopies } from '@/features/chat/attachments/pickers';
import { sweepExportTemp } from '@/features/chat/export/shareTranscript';
import { registerWipeHook } from '@/security/endSession';

/**
 * Plaintext attachment files of the active workspace (WS-SEC-2): picker
 * copies, opened downloads, staged upload chunks and transcript exports, all
 * in the app's caches directory. Only the active workspace can create them,
 * so they are deleted whenever it is put aside or ended: a switch, which
 * deletes nothing else, would otherwise leave them behind until the next cold
 * start, and so would removing the workspace later from the switcher (that
 * wipe never touches the caches directory). Best effort, never throws.
 */
export function sweepAttachmentTemp(): void {
  sweepPickerCopies();
  sweepDownloadTemp();
  sweepUploadTemp();
  sweepExportTemp();
}

registerWipeHook('attachment_temp', sweepAttachmentTemp);
