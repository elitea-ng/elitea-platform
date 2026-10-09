import { useCallback, useState } from 'react';

import { trySerializePipelineYaml } from '../dumpYaml.helpers';

export interface PipelineYamlSerializationState {
  /** Why the last flow document could not be written as YAML; `undefined` once one could. */
  readonly serializationError: string | undefined;
  /** The document's YAML text, or `undefined` when the strict serializer refuses it. */
  readonly serializeDocument: (document: unknown) => string | undefined;
}

/**
 * The editor's strict write path. A document the serializer refuses is never stored: the caller keeps
 * the stored text and graph, and shows `serializationError`. Storing the old "Error dumping YAML: …"
 * string as the document made admission read a graph of 0 nodes and blocked Save (pipeline 165).
 */
export function usePipelineYamlSerialization(): PipelineYamlSerializationState {
  const [serializationError, setSerializationError] = useState<string | undefined>(undefined);
  const serializeDocument = useCallback((document: unknown): string | undefined => {
    const result = trySerializePipelineYaml(document);
    setSerializationError(result.error);
    return result.yaml;
  }, []);
  return { serializationError, serializeDocument };
}
