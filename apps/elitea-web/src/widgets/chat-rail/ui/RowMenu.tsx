/**
 * A row's hover "⋯": the row's actions (`ContextAction`), the same list the
 * desktop shows as the native context menu. A submenu opens to the side.
 */
import { useState } from 'react';

import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';

import { MenuDivider, MenuRow, MenuRowText, NativeSubmenu, pressToOpen } from '@/shared/ui/native-menu';

import type { ContextAction, ContextLeaf } from '../model/contextAction';

function Leaf({ item, close }: { readonly item: ContextLeaf; readonly close: () => void }): React.JSX.Element {
  return (
    <MenuRow
      disabled={item.enabled === false}
      onClick={() => {
        close();
        item.run();
      }}
    >
      <MenuRowText>{item.label}</MenuRowText>
    </MenuRow>
  );
}

export function RowMenu({ items, label, testId = 'shell-conversation-more' }: { readonly items: readonly ContextAction[]; readonly label: string; readonly testId?: string }): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  return (
    <>
      <IconButton size="small" aria-label={label} aria-haspopup="menu" data-testid={testId} {...pressToOpen(setAnchor)}>
        <MoreHorizIcon fontSize="inherit" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true }, paper: { sx: { minWidth: '12rem' } } }}>
        {items.map((item, index) => {
          if ('separator' in item) return <MenuDivider key={`separator-${String(index)}`} />;
          if ('submenu' in item) {
            return (
              <NativeSubmenu key={item.label} label={item.label} disabled={item.enabled === false} minWidth="10rem">
                {(closeSubmenu) =>
                  item.submenu.map((child) => (
                    <Leaf
                      key={child.id}
                      item={child}
                      close={() => {
                        closeSubmenu();
                        close();
                      }}
                    />
                  ))
                }
              </NativeSubmenu>
            );
          }
          return <Leaf key={item.id} item={item} close={close} />;
        })}
      </Menu>
    </>
  );
}
