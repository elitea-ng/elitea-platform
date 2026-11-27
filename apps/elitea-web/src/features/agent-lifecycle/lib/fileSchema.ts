/**
 * The format marker of an agent or team file (issue 3137): `schema:
 * elitea.agent/v1` in an `.agent.md` front matter, `schema: elitea.team/v1`
 * in a `team.yaml`. A file without the key is a pre-v1 file and imports as it
 * always did; a file naming another format is refused, in words, before any
 * of it is read, because guessing at a newer format would import it wrong.
 */
import { t } from '@/shared/i18n';

import { ImportDocumentError } from './importError';

export const AGENT_FILE_SCHEMA = 'elitea.agent/v1';
export const TEAM_FILE_SCHEMA = 'elitea.team/v1';

/** Throws when `front.schema` is present and is not `expected`. */
export function checkFileSchema(front: Readonly<Record<string, unknown>>, expected: string): void {
  const schema = front['schema'];
  if (schema === undefined || schema === null || schema === expected) return;
  throw new ImportDocumentError(
    t('features.agentLifecycle.import.unknownSchema', 'This file uses format {{schema}}, which this version of {{product}} cannot read.', {
      schema: typeof schema === 'string' ? schema : JSON.stringify(schema),
    }),
  );
}
