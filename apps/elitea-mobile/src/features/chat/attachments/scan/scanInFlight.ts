/**
 * One document scan at a time, app-wide: from the attach menu to the file in the tray (the
 * camera, the format sheet, the PDF or text). VisionKit's scratch directory
 * (com.apple.DocumentCamera) is shared by every scan, so it may only be deleted when no scan is
 * running: a second camera capturing into it would lose its pages.
 */
let running = false;

export function scanInFlight(): boolean {
  return running;
}

/** Claims the scan; answers the release, or null when a scan is running already. */
export function beginScan(): (() => void) | null {
  if (running) return null;
  running = true;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    running = false;
  };
}

export const SCAN_ALREADY_RUNNING = 'A scan is already being prepared.';
