import { randomUUID } from 'expo-crypto';
import { Directory, File, Paths } from 'expo-file-system';
import { useEffect, useRef, useState } from 'react';
import { AccessibilityInfo, ActionSheetIOS } from 'react-native';

import type { AttachmentLimits } from '@/features/chat/attachments/prepare';
import { runScan, type ScanOutcome } from '@/features/chat/attachments/scan/scanFlow';
import { SCAN_CACHE_DIR, VISIONKIT_CACHE_DIR, type ScanFormat } from '@/features/chat/attachments/scan/scanPipeline';
import { beginScan, SCAN_ALREADY_RUNNING } from '@/features/chat/attachments/scan/scanInFlight';
import { documentScanner, scanSource, type ScanSource } from '@/features/chat/attachments/scan/scanner';

function remove(uri: string): void {
  try {
    const file = new File(uri);
    if (file.exists) file.delete();
  } catch {
    // swept with the DocumentScan directory (pickers.ts)
  }
}

function makeDirectory() {
  const dir = new Directory(Paths.cache, SCAN_CACHE_DIR, randomUUID());
  dir.create({ intermediates: true, idempotent: true });
  return {
    uri: dir.uri.endsWith('/') ? dir.uri : `${dir.uri}/`,
    remove: () => {
      try {
        if (dir.exists) dir.delete();
      } catch {
        // swept at the next start
      }
    },
  };
}

/** VisionKit's own scratch directory in the app's caches: nothing of a finished scan stays there. */
function sweepVisionKitScratch(): void {
  try {
    const dir = new Directory(Paths.cache, VISIONKIT_CACHE_DIR);
    if (dir.exists) dir.delete();
  } catch {
    // swept at the next start (pickers.ts)
  }
}

const FORMAT_LABEL: Record<ScanFormat, (pages: number) => string> = {
  pdf: (pages) => (pages === 1 ? 'PDF (1 page)' : `PDF (${pages} pages)`),
  text: () => 'Text only',
};

/** The system action sheet: "Attach the scan as" PDF / Text only / Cancel. */
function chooseFormat(formats: ScanFormat[], pageCount: number): Promise<ScanFormat | null> {
  return new Promise((resolve) => {
    ActionSheetIOS.showActionSheetWithOptions(
      {
        title: 'Attach the scan as',
        message: 'Both carry the text recognised on this device. Text only is smaller: no page images.',
        options: [...formats.map((f) => FORMAT_LABEL[f](pageCount)), 'Cancel'],
        cancelButtonIndex: formats.length,
      },
      (index) => resolve(index >= 0 && index < formats.length ? formats[index] : null),
    );
  });
}

/**
 * The thread's document scan: whether it is offered here (`source`), and `scan()`, which
 * answers the files for the tray. iOS only; Android (ML Kit) is #20.
 *
 * `phase` is what the screen shows between the attach menu and the tray: `scanning` while the
 * camera or the format sheet is up, `preparing` while the PDF or text is made (a placeholder in
 * the tray, announced to VoiceOver). While it is not `idle` the screen holds a send (it would
 * go without the scan) and another attach.
 */
export type ScanPhase = 'idle' | 'scanning' | 'preparing';

export const PREPARING_ANNOUNCEMENT = 'Preparing the scan…';

export function useDocumentScan(
  limits: AttachmentLimits,
  /** Development previews answer the format sheet themselves. */
  options: { chooseFormat?: (formats: ScanFormat[], pageCount: number) => Promise<ScanFormat | null> } = {},
): { source: ScanSource; phase: ScanPhase; scan: (held?: () => { count: number; totalBytes: number }) => Promise<ScanOutcome> } {
  const [native] = useState(() => documentScanner());
  const [phase, setPhase] = useState<ScanPhase>('idle');
  const source = scanSource(native);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  /** `held`: what the tray carries already (its `usage`), so the scan is fitted to the room left. */
  const scan = async (held?: () => { count: number; totalBytes: number }): Promise<ScanOutcome> => {
    if (!native || source === null) return { kind: 'cancelled' };
    // One at a time (the screen also disables Attach meanwhile): a second camera would capture
    // into the scratch directory this scan deletes when it ends.
    const end = beginScan();
    if (end === null) return { kind: 'refused', message: SCAN_ALREADY_RUNNING };
    setPhase('scanning');
    try {
      return await runScan({
        source,
        native,
        limits,
        makeDirectory,
        chooseFormat: options.chooseFormat ?? chooseFormat,
        // eslint-disable-next-line @typescript-eslint/no-require-imports -- development builds only: kept out of release bundles
        samplePages: __DEV__ ? require('@/features/chat/attachments/scan/scanSamples').samplePages : undefined,
        remove,
        isCurrent: () => mounted.current,
        held,
        onPreparing: () => {
          if (!mounted.current) return;
          setPhase('preparing');
          AccessibilityInfo.announceForAccessibility(PREPARING_ANNOUNCEMENT);
        },
      });
    } finally {
      end();
      // No scan is running now (the guard above), so nothing is capturing into it.
      sweepVisionKitScratch();
      if (mounted.current) setPhase('idle');
    }
  };
  return { source, phase, scan };
}
