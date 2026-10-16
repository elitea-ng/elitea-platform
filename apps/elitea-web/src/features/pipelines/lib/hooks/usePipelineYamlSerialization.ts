import { useCallback, useState } from 'react';

import { trySerializePipelineYaml, type DumpYamlOptions } from '../dumpYaml.helpers';

export interface PipelineYamlSerializationState {
  /** Why the last flow document could not be written as YAML, while the editor still holds what it kept. */
  readonly serializationError: string | undefined;
  /** The document's YAML text, or `undefined` when the strict serializer refuses it. `originalYaml` keeps unchanged source bytes. */
  readonly serializeDocument: (document: unknown, options?: DumpYamlOptions) => string | undefined;
  /** Surface a refusal raised by a store edit that serializes internally with the same strict serializer. */
  readonly reportSerializationError: (caught: unknown) => void;
}

interface Refusal {
  readonly error: string;
  readonly yamlCode: string;
  readonly yamlJsonObject: unknown;
}

/**
 * The editor's strict write path. A document the serializer refuses is never stored: the caller keeps
 * the stored text and graph, and shows `serializationError`. Storing the old "Error dumping YAML: …"
 * string as the document made admission read a graph of 0 nodes and blocked Save (pipeline 165).
 *
 * The refusal describes the document the editor kept, so it lapses as soon as that document is
 * replaced: typing, Cancel, a version switch, or a later successful write.
 */
export function usePipelineYamlSerialization(yamlCode: string, yamlJsonObject: unknown): PipelineYamlSerializationState {
  const [refusal, setRefusal] = useState<Refusal | undefined>(undefined);
  const serializeDocument = useCallback(
    (document: unknown, options?: DumpYamlOptions): string | undefined => {
      const result = trySerializePipelineYaml(document, options);
      setRefusal(result.error === undefined ? undefined : { error: result.error, yamlCode, yamlJsonObject });
      return result.yaml;
    },
    [yamlCode, yamlJsonObject],
  );
  const reportSerializationError = useCallback(
    (caught: unknown): void => {
      setRefusal({ error: caught instanceof Error ? caught.message : String(caught), yamlCode, yamlJsonObject });
    },
    [yamlCode, yamlJsonObject],
  );
  const current = refusal?.yamlCode === yamlCode && refusal.yamlJsonObject === yamlJsonObject;
  return { serializationError: current ? refusal.error : undefined, serializeDocument, reportSerializationError };
}
