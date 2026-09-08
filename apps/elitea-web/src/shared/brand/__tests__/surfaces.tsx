import Accordion from '@mui/material/Accordion';
import AccordionDetails from '@mui/material/AccordionDetails';
import AccordionSummary from '@mui/material/AccordionSummary';
import Alert from '@mui/material/Alert';
import AppBar from '@mui/material/AppBar';
import Autocomplete from '@mui/material/Autocomplete';
import Avatar from '@mui/material/Avatar';
import Badge from '@mui/material/Badge';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import Chip from '@mui/material/Chip';
import CssBaseline from '@mui/material/CssBaseline';
import Dialog from '@mui/material/Dialog';
import Drawer from '@mui/material/Drawer';
import FormControl from '@mui/material/FormControl';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormHelperText from '@mui/material/FormHelperText';
import IconButton from '@mui/material/IconButton';
import Input from '@mui/material/Input';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Paper from '@mui/material/Paper';
import Radio from '@mui/material/Radio';
import Select from '@mui/material/Select';
import Switch from '@mui/material/Switch';
import Tab from '@mui/material/Tab';
import TablePagination from '@mui/material/TablePagination';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { DataGrid } from '@mui/x-data-grid';

import { App } from '@/app/App';

import { muiOverrides } from '../mui-overrides';
import OutlinedInput from '@mui/material/OutlinedInput';

/**
 * The render surface for §4.6 check 7 assertion (c).
 *
 * The spec's wording is "the full route tree". There is no route tree yet —
 * unit R1 owns it and has not landed — so this registry is the honest
 * substitute AND the mechanism that makes the sweep widen by itself:
 *
 *  - `OVERRIDE_SURFACES` must contain an entry for every key
 *    `muiOverrides()` wires. The contract test asserts that, so the day unit
 *    S1 adds `MuiTextField` to the override package, this test fails until a
 *    TextField surface is added here. The sweep cannot silently stay narrow.
 *  - `ALWAYS_ON_SURFACES` covers the palette-driven MUI surfaces that render
 *    regardless of our overrides — in particular `Alert`, which is where
 *    `palette.error` / `palette.success` reach the DOM.
 *  - `APP_SURFACES` renders whatever application shell currently exists.
 *
 * REMOVER: unit R1 — when the route tree lands, add it here (or replace
 * APP_SURFACES with a router render) so the sweep covers real routes.
 *
 * [S1] Widened from 2 to 30 entries — one per key `muiOverrides()` now
 * wires (T1's original `MuiButton`/`MuiChip` plus the 27 S1-owned keys and
 * `MuiTypography`; `MuiTreeItem` is deliberately unwired, see
 * `mui-overrides/MuiTreeItem.ts`). A handful of components need real,
 * interactive state (`Menu`/`Select` need `open`; `Tooltip` needs a forced
 * open) to actually mount their styled slot rather than rendering nothing —
 * `ControlledSurfaces` below owns that state.
 */

const OVERRIDE_SURFACES: Record<string, () => React.ReactElement> = {
  // Two siblings under one parent (no per-item wrapper) exercises the
  // `:first-of-type`/`:last-of-type` selectors `MuiAccordion.ts` overrides;
  // `expanded` on the first one also exercises `.Mui-expanded`.
  MuiAccordion: () => (
    <>
      <Accordion expanded>
        <AccordionSummary>accordion one</AccordionSummary>
        <AccordionDetails>details one</AccordionDetails>
      </Accordion>
      <Accordion>
        <AccordionSummary>accordion two</AccordionSummary>
        <AccordionDetails>details two</AccordionDetails>
      </Accordion>
    </>
  ),
  MuiButton: () => (
    <>
      <Button variant="contained">contained</Button>
      <Button variant="secondary">secondary</Button>
      <Button variant="special">special</Button>
      <Button variant="auxiliary">auxiliary</Button>
      <Button variant="iconCounter">iconCounter</Button>
      <Button variant="maxi">maxi</Button>
      <Button variant="contained" disabled>
        disabled
      </Button>
      {/* [S1 Part B] the eight variants added alongside T1's original six. */}
      <Button variant="iconLabel">iconLabel</Button>
      <Button variant="tertiary">tertiary</Button>
      <Button variant="alarm">alarm</Button>
      <Button
        variant="elitea"
        color="primary"
      >
        elitea primary
      </Button>
      <Button
        variant="elitea"
        color="secondary"
      >
        elitea secondary
      </Button>
      <Button
        variant="elitea"
        color="tertiary"
      >
        elitea tertiary
      </Button>
      <Button
        variant="elitea"
        color="alarm"
      >
        elitea alarm
      </Button>
      <Button variant="text">text</Button>
      <Button variant="icon">icon</Button>
      <Button variant="neutral">neutral</Button>
      <Button variant="positive">positive</Button>
    </>
  ),
  MuiChip: () => (
    <>
      <Chip label="filled" />
      <Chip label="outlined" variant="outlined" />
    </>
  ),
  MuiAlert: () => (
    <>
      <Alert severity="error" variant="filled">
        error
      </Alert>
      <Alert severity="success" variant="filled">
        success
      </Alert>
      <Alert severity="warning" variant="filled">
        warning
      </Alert>
      <Alert severity="info" variant="filled">
        info
      </Alert>
    </>
  ),
  MuiAppBar: () => <AppBar position="static">app bar</AppBar>,
  MuiAutocomplete: () => (
    <Autocomplete
      open
      options={['one', 'two']}
      renderInput={(params) => (
        <TextField
          {...params}
          label="autocomplete"
        />
      )}
      sx={{ width: 200 }}
    />
  ),
  MuiAvatar: () => <Avatar>EL</Avatar>,
  MuiBadge: () => (
    <Badge badgeContent={4}>
      <Chip label="badged" />
    </Badge>
  ),
  MuiCheckbox: () => (
    <>
      <Checkbox checked />
      <Checkbox indeterminate />
      <Checkbox />
    </>
  ),
  MuiCssBaseline: () => <CssBaseline />,
  MuiDataGrid: () => (
    <div style={{ height: 160, width: 240 }}>
      <DataGrid
        columns={[{ field: 'id', headerName: 'ID' }]}
        rows={[{ id: 1 }, { id: 2 }]}
        hideFooter
      />
    </div>
  ),
  MuiDialog: () => (
    <Dialog open>
      <div style={{ padding: 8 }}>dialog</div>
    </Dialog>
  ),
  MuiDrawer: () => (
    <>
      <Drawer
        open
        variant="permanent"
        anchor="left"
      >
        <div style={{ padding: 8 }}>drawer left</div>
      </Drawer>
      <Drawer
        open
        variant="permanent"
        anchor="right"
      >
        <div style={{ padding: 8 }}>drawer right</div>
      </Drawer>
    </>
  ),
  MuiFormControl: () => (
    <FormControl error>
      <FormHelperText>form control</FormHelperText>
    </FormControl>
  ),
  MuiFormControlLabel: () => <FormControlLabel control={<Checkbox />} label="form control label" />,
  MuiFormHelperText: () => <FormHelperText error>helper text</FormHelperText>,
  MuiIconButton: () => (
    <>
      <IconButton color="primary">
        <span>p</span>
      </IconButton>
      <IconButton color="secondary">
        <span>s</span>
      </IconButton>
      <IconButton color="tertiary">
        <span>t</span>
      </IconButton>
      <IconButton color="alarm">
        <span>a</span>
      </IconButton>
    </>
  ),
  MuiInput: () => <Input defaultValue="input" />,
  MuiMenu: () => (
    <Menu
      open
      anchorEl={document.body}
    >
      <MenuItem>menu item</MenuItem>
    </Menu>
  ),
  MuiMenuItem: () => (
    <MenuList>
      <MenuItem>item</MenuItem>
      <MenuItem selected>selected</MenuItem>
    </MenuList>
  ),
  MuiOutlinedInput: () => <OutlinedInput defaultValue="outlined input" />,
  MuiList: () => (
    <MenuList>
      <MenuItem>item</MenuItem>
    </MenuList>
  ),
  MuiPaper: () => <Paper elevation={0}>elitea paper</Paper>,
  MuiRadio: () => (
    <>
      <Radio checked />
      <Radio />
    </>
  ),
  MuiSelect: () => (
    <Select
      variant="standard"
      open
      value="a"
      onChange={() => {}}
    >
      <MenuItem value="a">a</MenuItem>
    </Select>
  ),
  MuiSwitch: () => (
    <>
      <Switch checked />
      <Switch />
    </>
  ),
  MuiTab: () => (
    <Tabs value={0}>
      <Tab label="one" />
      <Tab label="two" />
      <Tab icon={<span>icon</span>} />
    </Tabs>
  ),
  MuiTablePagination: () => (
    <table>
      <tbody>
        <tr>
          <TablePagination
            component="td"
            count={10}
            page={0}
            rowsPerPage={5}
            onPageChange={() => {}}
          />
        </tr>
      </tbody>
    </table>
  ),
  MuiTabs: () => (
    <Tabs value={0}>
      <Tab label="one" />
    </Tabs>
  ),
  MuiTextField: () => (
    <TextField
      variant="standard"
      label="text field"
      defaultValue="value"
    />
  ),
  MuiToggleButton: () => (
    <ToggleButtonGroup value="a">
      <ToggleButton value="a">a</ToggleButton>
      <ToggleButton value="b">b</ToggleButton>
    </ToggleButtonGroup>
  ),
  MuiTooltip: () => (
    <Tooltip
      title="tooltip"
      open
    >
      <span>anchor</span>
    </Tooltip>
  ),
  MuiTypography: () => (
    <>
      <Typography variant="headingLarge">headingLarge</Typography>
      <Typography variant="headingMedium">headingMedium</Typography>
      <Typography variant="headingSmall">headingSmall</Typography>
    </>
  ),
};

const ALWAYS_ON_SURFACES: Record<string, () => React.ReactElement> = {
  Paper: () => <Paper elevation={2}>paper</Paper>,
  Alert: () => (
    <>
      <Alert severity="error" variant="filled">
        error
      </Alert>
      <Alert severity="success" variant="filled">
        success
      </Alert>
      <Alert severity="warning" variant="standard">
        warning
      </Alert>
      <Alert severity="info" variant="outlined">
        info
      </Alert>
    </>
  ),
  Typography: () => (
    <>
      <Typography variant="headingLarge">headingLarge</Typography>
      <Typography variant="bodyMedium">bodyMedium</Typography>
      <Typography variant="subtitle">subtitle</Typography>
    </>
  ),
};

const APP_SURFACES: Record<string, () => React.ReactElement> = {
  App: () => <App />,
};

/** Every surface, in a stable order. */
export function AllSurfaces() {
  const entries = [
    ...Object.entries(OVERRIDE_SURFACES),
    ...Object.entries(ALWAYS_ON_SURFACES),
    ...Object.entries(APP_SURFACES),
  ];
  return (
    <div>
      {entries.map(([name, Surface]) => (
        <section
          key={name}
          data-surface={name}
        >
          <Surface />
        </section>
      ))}
    </div>
  );
}

/** Keys the sweep claims to cover — asserted against `muiOverrides()`. */
export const COVERED_OVERRIDE_KEYS = Object.keys(OVERRIDE_SURFACES);

export const WIRED_OVERRIDE_KEYS = Object.keys(muiOverrides());
