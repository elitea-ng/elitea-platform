import type { StaticPipelinePauseProof, StaticPipelineToolInventory } from '@/shared/api/generated/model';

export type StaticPauseBinding = {
  readonly messageId: string;
  readonly generation: string;
  readonly threadId: string;
} & (
  | { readonly kind: "root"; readonly proof: StaticPipelinePauseProof }
  | { readonly kind: "tools"; readonly inventory: StaticPipelineToolInventory }
);
