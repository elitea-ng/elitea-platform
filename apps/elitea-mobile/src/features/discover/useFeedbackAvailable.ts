import { speaksContract } from '@/api/discovery';
import { MARKETPLACE_FEEDBACK_CONTRACT } from '@/api/queries/marketplaceFeedback';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** The listing feedback routes need client contract 1.55. */
export function useFeedbackAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, MARKETPLACE_FEEDBACK_CONTRACT.major, MARKETPLACE_FEEDBACK_CONTRACT.minor);
}
