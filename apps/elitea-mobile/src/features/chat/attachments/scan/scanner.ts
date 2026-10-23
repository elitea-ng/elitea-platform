import { requireOptionalNativeModule } from 'expo-modules-core';
import { Platform } from 'react-native';

/**
 * The DocumentScanner inline module (native-modules/DocumentScanner.swift), iOS only.
 * Every path it takes or returns is a file URI inside the app's caches directory.
 */
export interface DocumentScannerNative {
  /** VisionKit says its document camera can run here. */
  readonly isSupported: boolean;
  /** The iOS simulator: VisionKit may say it is supported there, but it cannot capture. */
  readonly isSimulator?: boolean;
  /**
   * Shows the document camera; resolves to the scanned pages (JPEG files written into
   * `directory`), or [] when the user cancels. Never writes to the photo library.
   */
  scan(directory: string): Promise<string[]>;
  /**
   * Recognises the pages' text on the device, once per scan: each page's text, and `layout`
   * (the lines and where they sit, opaque here) for `makePdf`'s text layer. Every size step
   * of the PDF reuses it, so the pages are not recognised again per step.
   */
  recognizeLayout(pages: string[]): Promise<{ layout: string; text: string[] }>;
  /**
   * Writes the pages into one PDF at `output`, fitted to `maxEdge` px and JPEG `quality`.
   * With `layout` (from `recognizeLayout`), the recognised text is laid over each page
   * invisibly, so the workspace can read the PDF as text.
   */
  makePdf(pages: string[], output: string, maxEdge: number, quality: number, layout: string | null): Promise<{ uri: string; size: number }>;
  /** Each page's text, recognised on the device. */
  recognizeText(pages: string[]): Promise<string[]>;
  /**
   * Writes `text` (UTF-8) to `output` with Complete file protection, like the pages and the
   * PDF; answers its size in bytes.
   */
  writeText(output: string, text: string): Promise<number>;
}

/** The native scanner, or null where there is none (Android: #20; Expo Go; web). */
export function documentScanner(
  os: typeof Platform.OS = Platform.OS,
  load: (name: string) => DocumentScannerNative | null = requireOptionalNativeModule,
): DocumentScannerNative | null {
  if (os !== 'ios') return null;
  return load('DocumentScanner');
}

/**
 * Where a scan's pages come from: the document camera, or (development builds on a device
 * without one, i.e. the simulator) bundled sample pages through the same pipeline. Null:
 * scanning is not offered.
 */
export type ScanSource = 'camera' | 'samples' | null;

export function scanSource(native: DocumentScannerNative | null = documentScanner(), dev: boolean = __DEV__): ScanSource {
  if (!native) return null;
  if (native.isSupported && native.isSimulator !== true) return 'camera';
  return dev ? 'samples' : null;
}
