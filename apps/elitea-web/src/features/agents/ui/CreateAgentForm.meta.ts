/**
 * `CreateAgentForm`'s version meta controls (issue 1895), split out for the
 * §3.5 file-length budget: whether the Advanced panel shows them, and the
 * server's refusal of each on the last save.
 */
import type { useCreateAgentFormState } from '../model/useCreateAgentFormState';

/** The version meta controls a page that saves the meta shows (issue 1895). */
export interface CreateAgentMetaControls {
  /** 422 `context_block_invalid` on the last save (`contextBlockRefusal`), for the Conversation history field. */
  readonly contextHistoryError?: string | undefined;
}

/** The Advanced panel's meta controls: shown only when the page gave `metaControls`. */
export function advanceMetaProps(state: ReturnType<typeof useCreateAgentFormState>, metaControls: CreateAgentMetaControls | undefined) {
  return {
    showOutputContract: metaControls !== undefined,
    contentControls: {
      citations: { value: state.citations, onChange: state.onCitationsChange },
      contextHistory: { value: state.contextHistory, onChange: state.onContextHistoryChange, error: metaControls?.contextHistoryError },
    },
  };
}
