/*
 * Teleport
 * Copyright (C) 2026  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

import {
  autoUpdate,
  flip as flipMiddleware,
  FloatingFocusManager,
  size as sizeMiddleware,
  FloatingList,
  FloatingNode,
  FloatingPortal,
  FloatingTree,
  offset as offsetMiddleware,
  Placement,
  safePolygon,
  shift as shiftMiddleware,
  useClick,
  useDismiss,
  useFloating,
  useFloatingNodeId,
  useFloatingParentNodeId,
  useFloatingTree,
  useHover,
  useInteractions,
  useListNavigation,
  useMergeRefs,
  useRole,
  useTransitionStyles,
} from '@floating-ui/react';
import {
  ElementType,
  forwardRef,
  ReactNode,
  RefCallback,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';

import { DropdownMenuContext } from 'design/DropdownMenu/DropdownMenuContext';
import { DropdownMenuPanel } from 'design/DropdownMenu/DropdownMenuPrimitives';

export type DropdownMenuProps = {
  renderTrigger?: (props: {
    ref: RefCallback<HTMLElement> | null;
    getReferenceProps: (
      userProps?: Record<string, unknown>
    ) => Record<string, unknown>;
    isOpen: boolean;
    isNested: boolean;
  }) => ReactNode;
  panelComponent?: ElementType;
  placement?: Placement;
  hoverTrigger?: boolean;
  hoverDelay?: { open?: number; close?: number };
  transition?: { duration?: number };
  offset?: number;
  onOpenChange?: (open: boolean) => void;
  children?: ReactNode;
};

const DEFAULT_HOVER_DELAY = { open: 100, close: 200 };
const DEFAULT_TRANSITION = { duration: 150 };
const DEFAULT_EDGE_PADDING = 8;

const MenuComponent = forwardRef<HTMLElement, DropdownMenuProps>(
  function MenuComponent(
    {
      renderTrigger,
      panelComponent,
      placement,
      hoverTrigger = false,
      hoverDelay = DEFAULT_HOVER_DELAY,
      transition = DEFAULT_TRANSITION,
      offset = 4,
      onOpenChange: onOpenChangeProp,
      children,
    },
    forwardedRef
  ) {
    const nodeId = useFloatingNodeId();
    const parentId = useFloatingParentNodeId();
    const isNested = parentId != null;
    const tree = useFloatingTree();

    const [isOpenState, setIsOpenState] = useState(false);
    const [activeIndex, setActiveIndex] = useState<number | null>(null);
    const [search, setSearch] = useState('');

    const elementsRef = useRef<(HTMLElement | null)[]>([]);
    const labelsRef = useRef<(string | null)[]>([]);

    const requestedPlacement =
      placement ?? (isNested ? 'right-start' : 'bottom-end');

    const {
      x,
      y,
      strategy,
      refs,
      context,
      placement: resolvedPlacement,
    } = useFloating({
      nodeId,
      placement: requestedPlacement,
      open: isOpenState,
      onOpenChange: open => {
        setIsOpenState(open);
        onOpenChangeProp?.(open);
        if (open && tree) {
          tree.events.emit('menuopen', { nodeId, parentId });
        }
        if (!open && !isNested) {
          setSearch('');
          setActiveIndex(null);
        }
      },
      middleware: [
        offsetMiddleware(offset),
        flipMiddleware(),
        shiftMiddleware({ padding: DEFAULT_EDGE_PADDING }),
        sizeMiddleware({
          padding: DEFAULT_EDGE_PADDING,
          // Prefer maxHeight/maxWidth props, but set lower if viewport is smaller.
          apply({ availableHeight, availableWidth, elements }) {
            const cs = getComputedStyle(elements.floating);
            const cssMaxH = parseFloat(cs.maxHeight);
            const cssMaxW = parseFloat(cs.maxWidth);
            const effectiveMaxH = Number.isFinite(cssMaxH)
              ? Math.min(cssMaxH, availableHeight)
              : availableHeight;
            const effectiveMaxW = Number.isFinite(cssMaxW)
              ? Math.min(cssMaxW, availableWidth)
              : availableWidth;
            elements.floating.style.maxHeight = `${effectiveMaxH}px`;
            elements.floating.style.maxWidth = `${effectiveMaxW}px`;
          },
        }),
      ],
      whileElementsMounted: autoUpdate,
    });

    const [side] = resolvedPlacement.split('-');
    const transformOrigin =
      { top: 'bottom', bottom: 'top', left: 'right', right: 'left' }[side] ??
      'top';

    const { isMounted, styles: transitionStyles } = useTransitionStyles(
      context,
      {
        duration: transition.duration,
        initial: { opacity: 0, transform: 'scale(0.96)' },
        common: { transformOrigin, transitionTimingFunction: 'ease' },
      }
    );

    const hover = useHover(context, {
      enabled: isNested || hoverTrigger,
      delay: hoverDelay,
      handleClose: safePolygon({ requireIntent: true }),
    });

    const click = useClick(context, {
      event: 'mousedown',
      toggle: !isNested,
      ignoreMouse: isNested,
    });

    const dismiss = useDismiss(context, { bubbles: true });
    const role = useRole(context, { role: 'menu' });

    const listNav = useListNavigation(context, {
      listRef: elementsRef,
      activeIndex,
      onNavigate: setActiveIndex,
      nested: isNested,
    });

    const { getReferenceProps, getFloatingProps, getItemProps } =
      useInteractions([hover, click, dismiss, role, listNav]);

    const mergedRef = useMergeRefs([refs.setReference, forwardedRef]);

    // Close sibling submenus when another opens
    useEffect(() => {
      if (!tree) return;

      function handleMenuOpen(event: { nodeId: string; parentId: string }) {
        if (event.parentId === parentId && event.nodeId !== nodeId) {
          setIsOpenState(false);
        }
      }

      tree.events.on('menuopen', handleMenuOpen);
      return () => {
        tree.events.off('menuopen', handleMenuOpen);
      };
    }, [tree, parentId, nodeId]);

    const closeMenu = useCallback(() => {
      setIsOpenState(false);
      onOpenChangeProp?.(false);
    }, [onOpenChangeProp]);

    const PanelComp = panelComponent ?? DropdownMenuPanel;

    const panel = (
      <PanelComp
        ref={refs.setFloating}
        style={{
          position: strategy,
          top: y ?? 0,
          left: x ?? 0,
          ...transitionStyles,
        }}
        {...getFloatingProps()}
      >
        {children}
      </PanelComp>
    );

    const contextValue = useMemo(
      () => ({
        getItemProps,
        activeIndex,
        setActiveIndex,
        isOpen: isOpenState,
        closeMenu,
        search,
        setSearch,
      }),
      [getItemProps, activeIndex, isOpenState, closeMenu, search]
    );

    return (
      <FloatingNode id={nodeId}>
        {renderTrigger?.({
          ref: mergedRef,
          getReferenceProps,
          isOpen: isOpenState,
          isNested,
        })}
        <DropdownMenuContext.Provider value={contextValue}>
          {isMounted && (
            <FloatingList elementsRef={elementsRef} labelsRef={labelsRef}>
              <FloatingPortal>
                {isNested ? (
                  panel
                ) : (
                  <FloatingFocusManager context={context} modal={false}>
                    {panel}
                  </FloatingFocusManager>
                )}
              </FloatingPortal>
            </FloatingList>
          )}
        </DropdownMenuContext.Provider>
      </FloatingNode>
    );
  }
);

/**
 * Composable dropdown menu with positioning/sizing, keyboard navigation,
 * hover interactions, and support for nested submenus.
 *
 * Provide a trigger via `renderTrigger` and menu content as children.
 * Menus can be nested — placing a `DropdownMenu` inside another
 * automatically enables submenu behavior (hover-to-open, arrow-key
 * navigation between levels).
 *
 * @example
 * ```tsx
 * <DropdownMenu
 *   placement="bottom-end"
 *   renderTrigger={({ ref, getReferenceProps }) => (
 *     <Button ref={ref} {...getReferenceProps()}>Open</Button>
 *   )}
 * >
 *   <DropdownMenuSection>
 *     <DropdownMenuItem label="Edit" onClick={handleEdit} />
 *     <DropdownMenuItem label="Delete" onClick={handleDelete} />
 *   </DropdownMenuSection>
 * </DropdownMenu>
 * ```
 */
export const DropdownMenu = forwardRef<HTMLElement, DropdownMenuProps>(
  function DropdownMenu(props, ref) {
    const parentId = useFloatingParentNodeId();
    if (parentId == null) {
      return (
        <FloatingTree>
          <MenuComponent ref={ref} {...props} />
        </FloatingTree>
      );
    }
    return <MenuComponent ref={ref} {...props} />;
  }
);
