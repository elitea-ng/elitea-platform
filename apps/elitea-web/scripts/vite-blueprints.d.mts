// Types for vite-blueprints.mjs (vite.config.ts imports it). Keep in step with it.
import type { Plugin } from 'vite';

export declare const BLUEPRINTS_DIR: string;
export declare function blueprintContentType(path: string): string;
export declare function blueprintRequestPath(url: string | undefined): string | undefined;
export declare function blueprintsPlugin(options?: {
  root?: string;
  build?: (root: string) => Promise<{ files: Map<string, Uint8Array | string> }>;
}): Plugin;
