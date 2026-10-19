/**
 * The canonical fan-out contract fixtures (`libs/jsonschema/runtime/v1/fixtures/fanout-*`),
 * read as they are: Main and the Worker validate the same files, so a Web test
 * built on them breaks when the contract does.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const FIXTURES = resolve(import.meta.dirname, '../../../../../../libs/jsonschema/runtime/v1/fixtures');

/** One fixture by file stem, e.g. `fanout-interrupt-card-v1.static-pause`. */
export function fanoutFixture<T = Record<string, unknown>>(stem: string): T {
  return JSON.parse(readFileSync(resolve(FIXTURES, `${stem}.json`), 'utf8')) as T;
}

/** The raw fixture bytes, for byte-exact assertions. */
export function fanoutFixtureText(stem: string): string {
  return readFileSync(resolve(FIXTURES, `${stem}.json`), 'utf8');
}
