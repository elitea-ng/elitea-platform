/* Test-only helper: reads an `.sse` fixture next to this file. */
// The app's tsconfig has no Node types; jest runs this file in Node.
declare const __dirname: string;
interface NodeFs {
  readFileSync(path: string): Uint8Array;
  readFileSync(path: string, encoding: 'utf8'): string;
}
// eslint-disable-next-line @typescript-eslint/no-require-imports
const fs = require('fs') as NodeFs;
const join = (dir: string, file: string) => `${dir}/${file}`;

export const FIXTURE_NAMES = [
  'simple-answer',
  'tool-call',
  'intermediate-response',
  'execution-failed',
  'llm-error',
  'hitl-terminal',
  'hitl-fanout-child',
  'mcp-authorization',
  'replay-reset',
] as const;

export type FixtureName = (typeof FIXTURE_NAMES)[number];

export function loadFixtureBytes(name: FixtureName): Uint8Array {
  return new Uint8Array(fs.readFileSync(join(__dirname, `${name}.sse`)));
}

export function loadFixtureText(name: FixtureName): string {
  return fs.readFileSync(join(__dirname, `${name}.sse`), 'utf8');
}
