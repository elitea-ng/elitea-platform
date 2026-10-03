import Alert from '@mui/material/Alert';
import Avatar from '@mui/material/Avatar';
import Chip from '@mui/material/Chip';
import DialogTitle from '@mui/material/DialogTitle';
import FormHelperText from '@mui/material/FormHelperText';
import ListItemText from '@mui/material/ListItemText';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Stack from '@mui/material/Stack';
import Tab from '@mui/material/Tab';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect } from 'storybook/test';

import { avatarInitialsType, typeScale } from '@/shared/brand/typeScale';

/**
 * Typography spec §4.5: the component OVERRIDES of the one type scale,
 * measured in a real browser. jsdom cannot answer this — it returns rem
 * strings, or nothing, for emotion's rules — so the assertion lives in the
 * `storybook` project (Chromium via @vitest/browser-playwright), next to the
 * computed-colour checks the other override stories already make.
 *
 * Every slot below rendered at one of MUI's own sizes (11/13/15/16/20/24px)
 * before the overrides; each must now compute to a rung of the default
 * pack's ladder.
 */
const LADDER_PX = new Set([12, 14, 16, 20]);

function Specimen() {
  return (
    <Stack
      spacing={2}
      sx={{ p: 2, width: 480 }}
    >
      <DialogTitle data-probe="dialog-title">Dialog title</DialogTitle>
      <MenuList>
        <MenuItem data-probe="menu-item">Menu item</MenuItem>
      </MenuList>
      <Chip
        data-probe="chip"
        label="Chip"
      />
      <Tabs
        value={0}
        aria-label="Specimen tabs"
      >
        <Tab
          data-probe="tab"
          label="Tab"
        />
      </Tabs>
      <Table>
        <TableHead>
          <TableRow>
            <TableCell data-probe="table-head">Head</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          <TableRow>
            <TableCell data-probe="table-body">Body</TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <TextField
        id="specimen-resting"
        label="Resting label"
        data-probe="field-resting"
      />
      <TextField
        id="specimen-shrunk"
        label="Shrunk label"
        defaultValue="value"
        data-probe="field-shrunk"
      />
      <FormHelperText data-probe="helper">Helper text</FormHelperText>
      <Alert
        severity="info"
        data-probe="alert"
      >
        Alert message
      </Alert>
      <ListItemText
        data-probe="list-item"
        primary="Primary line"
        secondary="Secondary line"
      />
      <Avatar data-probe="avatar-40">EL</Avatar>
      <Avatar
        data-probe="avatar-24"
        sx={(theme) => ({ width: 24, height: 24, ...typeScale(theme.typography[avatarInitialsType(24)]) })}
      >
        EL
      </Avatar>
    </Stack>
  );
}

const meta = {
  title: 'shared/brand/TypeScale',
  component: Specimen,
  parameters: { a11y: { test: 'error' } },
} satisfies Meta<typeof Specimen>;

export default meta;
type Story = StoryObj<typeof meta>;

const px = (element: Element): number => Number.parseFloat(getComputedStyle(element).fontSize);

function probe(root: HTMLElement, name: string, selector?: string): Element {
  const host = root.querySelector(`[data-probe="${name}"]`);
  if (!host) throw new Error(`no probe ${name}`);
  const target = selector ? host.querySelector(selector) : host;
  if (!target) throw new Error(`probe ${name} has no ${selector}`);
  return target;
}

export const OverridesStayOnTheLadder: Story = {
  play: async ({ canvasElement }) => {
    const slots: [string, string | undefined, number][] = [
      ['dialog-title', undefined, 16],
      ['menu-item', undefined, 14],
      ['chip', 'span', 12],
      ['tab', undefined, 14],
      ['table-head', undefined, 14],
      ['table-body', undefined, 14],
      ['field-resting', 'label', 14],
      ['field-resting', 'input', 14],
      ['field-shrunk', 'label', 14],
      ['field-shrunk', 'legend', 12],
      ['helper', undefined, 12],
      ['alert', ':scope > div:last-child', 14],
      ['list-item', 'span', 14],
      ['list-item', 'p', 12],
      ['avatar-40', undefined, 16],
      ['avatar-24', undefined, 12],
    ];
    for (const [name, selector, expected] of slots) {
      const size = px(probe(canvasElement, name, selector));
      await expect(LADDER_PX.has(size), `${name} ${selector ?? ''} = ${size}px`).toBe(true);
      await expect(size, `${name} ${selector ?? ''}`).toBe(expected);
    }
  },
};

export const ShrunkLabelLandsOnRungMinusOne: Story = {
  play: async ({ canvasElement }) => {
    // The label keeps its 14px font and is scaled by k = 12/14; its RENDERED
    // glyph height must therefore match the 12px legend that cuts the notch.
    const label = probe(canvasElement, 'field-shrunk', 'label');
    const transform = getComputedStyle(label).transform;
    const scale = Number(/matrix\(([^,]+)/.exec(transform)?.[1]);
    await expect(Math.round(14 * scale)).toBe(12);
    const resting = probe(canvasElement, 'field-resting', 'label');
    await expect(getComputedStyle(resting).transform).toMatch(/matrix\(1, 0, 0, 1,/);
  },
};
