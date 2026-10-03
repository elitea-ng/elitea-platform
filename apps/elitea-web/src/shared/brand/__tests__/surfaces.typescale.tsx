import DialogTitle from '@mui/material/DialogTitle';
import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import ListItemText from '@mui/material/ListItemText';
import OutlinedInput from '@mui/material/OutlinedInput';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';

/**
 * Render surfaces for the four override keys the typography spec (rev. 2)
 * added — merged into `OVERRIDE_SURFACES` in `surfaces.tsx`, so the §4.6
 * check 7 sweep still covers every key `muiOverrides()` wires.
 */
export const TYPE_SCALE_SURFACES: Record<string, () => React.ReactElement> = {
  MuiDialogTitle: () => <DialogTitle>dialog title</DialogTitle>,
  MuiInputLabel: () => (
    <FormControl variant="outlined">
      <InputLabel shrink>shrunk label</InputLabel>
      <OutlinedInput
        label="shrunk label"
        notched
      />
    </FormControl>
  ),
  MuiListItemText: () => (
    <ListItemText
      primary="primary"
      secondary="secondary"
    />
  ),
  MuiTableCell: () => (
    <Table>
      <TableHead>
        <TableRow>
          <TableCell>head</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        <TableRow>
          <TableCell>body</TableCell>
        </TableRow>
      </TableBody>
    </Table>
  ),
};
