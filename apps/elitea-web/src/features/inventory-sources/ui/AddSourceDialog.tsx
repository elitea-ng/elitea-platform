/**
 * Choosing a source toolkit to add.
 *
 * IT LISTS THE PROJECT'S OWN TOOLKITS, filtered to the types the descriptor
 * admits as sources, minus the ones already added. Typing an id would be the
 * smaller change and the worse screen: the ids are integers nobody remembers,
 * a mistyped one is accepted by the save and refused by the facade at ingest
 * time — hours later, with a message about a toolkit that is not a source of
 * this Inventory toolkit.
 *
 * A PROJECT WITH NO CANDIDATE IS ITS OWN MESSAGE. "No toolkit to add" and
 * "every toolkit is already a source" are different facts, and only the first
 * one tells the user to go and create a repository toolkit.
 */
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

/** One toolkit that may become a source. */
export interface SourceCandidate {
  readonly id: string;
  readonly name: string;
  readonly type: string;
}

export interface AddSourceDialogProps {
  readonly open: boolean;
  readonly candidates: readonly SourceCandidate[];
  /** True when every eligible toolkit is already a source of this inventory. */
  readonly allAdded: boolean;
  readonly onAdd: (toolkitId: string) => void;
  readonly onClose: () => void;
}

export function AddSourceDialog({
  open,
  candidates,
  allAdded,
  onAdd,
  onClose,
}: AddSourceDialogProps): React.JSX.Element {
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>{t('inventory.sources.addTitle', 'Add source')}</DialogTitle>
      <DialogContent data-testid="inventory-add-source-dialog">
        {candidates.length === 0 ? (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="inventory-no-candidates">
            {allAdded
              ? t('inventory.sources.allAdded', 'Every repository toolkit in this project is already a source.')
              : t(
                  'inventory.sources.noCandidates',
                  'This project has no repository toolkit to ingest from. Create a GitHub, Azure DevOps, GitLab or Bitbucket toolkit first.',
                )}
          </Typography>
        ) : (
          <List dense>
            {candidates.map((candidate) => (
              <ListItemButton
                key={candidate.id}
                data-testid="inventory-source-candidate"
                data-toolkit-id={candidate.id}
                onClick={() => {
                  onAdd(candidate.id);
                }}
              >
                <ListItemText primary={candidate.name} secondary={`${candidate.type} · ${candidate.id}`} />
              </ListItemButton>
            ))}
          </List>
        )}
      </DialogContent>
      <DialogActions>
        <BaseBtn variant="text" size="small" onClick={onClose}>
          {t('inventory.sources.addCancel', 'Cancel')}
        </BaseBtn>
      </DialogActions>
    </Dialog>
  );
}
