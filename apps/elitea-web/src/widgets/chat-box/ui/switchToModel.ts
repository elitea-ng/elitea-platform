/**
 * The panel's "X": leave the agent or pipeline and talk to the model.
 *
 * It used to be `selectSavedOrDefaultModel` alone, which only picks the model
 * the composer would use; the active participant stayed set, so the panel
 * still read "To: <agent>" and the next send still went to it.
 */
export function switchToModel(
  onChangeParticipant: (participant: unknown) => void,
  selectSavedOrDefaultModel: () => void,
): () => void {
  return () => {
    onChangeParticipant(undefined);
    selectSavedOrDefaultModel();
  };
}
