/**
 * A model-catalogue row -> the `LLMModel` shape this widget's own controls
 * read.
 *
 * It used to live beside its first caller, in
 * `widgets/chat-box/ui/ChatBox.helpers.ts`. It moved here because the
 * catalogue row and `LLMModel` are the two ends of one adapter and
 * `LLMModel` is this slice's type: the second caller —
 * `widgets/agent-model-settings`, which pins the model an agent version runs
 * on — cannot reach a helper inside `chat-box` without either a deep
 * cross-slice import (refused by `.dependency-cruiser.cjs`'s
 * `no-deep-slice-import-cross-slice`) or an import of the `chat-box` barrel,
 * which would drag `ChatBox` itself into a form. Moved verbatim: the field
 * mapping below is unchanged.
 */
import type { ConfigModel } from '@/shared/api/configurationsApi';

import type { LLMModel } from './types';

/**
 * Every optional field is spread conditionally rather than assigned, because
 * `exactOptionalPropertyTypes` rejects `display_name: undefined` against
 * `LLMModel`'s own optional key. The bracket reads are the catalogue's
 * untyped tail (`ConfigModel` declares an index signature for it), so each
 * one is `typeof`-checked before it is carried.
 */
export function toLlmModel(raw: ConfigModel): LLMModel {
  return {
    id: raw.id !== undefined ? String(raw.id) : raw.name,
    name: raw.name,
    ...(raw.display_name !== undefined ? { display_name: raw.display_name } : {}),
    // A blank description is no description: the menu must not leave an
    // empty second line under the name.
    ...(typeof raw['description'] === 'string' && raw['description'].trim() !== ''
      ? { description: raw['description'].trim() }
      : {}),
    ...(typeof raw['shared'] === 'boolean' ? { shared: raw['shared'] } : {}),
    ...(typeof raw['supports_vision'] === 'boolean' ? { supports_vision: raw['supports_vision'] } : {}),
    ...(typeof raw['supports_reasoning'] === 'boolean' ? { supports_reasoning: raw['supports_reasoning'] } : {}),
    ...(typeof raw['max_output_tokens'] === 'number' ? { max_output_tokens: raw['max_output_tokens'] } : {}),
  };
}
