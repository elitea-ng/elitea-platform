/**
 * EliteaAssistant — the floating support widget's root.
 *
 * Ported from `@eliteaai/elitea-assistant`'s `src/EliteaAssistant.tsx`, with the
 * embedding seams removed rather than kept as dead props.
 *
 * The published component takes `apiUrl`, `token`, `withCredentials`,
 * `socketPath` and an optional `apiAdapter`, because it is designed to be
 * dropped into ANY host application and must be told how to reach its backend.
 * Inside this app there is exactly one answer to all of that — `eliteaFetch`,
 * the app's session cookie, and this deployment's API origin — so the props are
 * gone and `createSupportApi()` is the adapter. Keeping them would be offering a
 * caller choices whose only correct value is the default.
 *
 * `SocketContext` is gone with them; the transport is `lib/hooks/stream.hook.ts`.
 */
import { forwardRef, useImperativeHandle, useMemo, useRef } from 'react';

import { productName } from '@/shared/brand';

import {
  ApiContext,
  SupportAssistantContextValue,
  ThemeContext,
  useAssistantState,
  useInitAssistant,
  usePopup,
} from './lib/hooks';
import { createSupportApi } from './api';
import { ChatButton, ChatWindow, PopupMessage } from './components/chat';
import type {
  TAssistantConfig,
  TEliteaAssistantColors,
  TEliteaAssistantRef,
  TSupportAssistantContext,
} from './lib/types';
import { colorsToCSSProperties, resolveColors } from './theme/colors.theme';
import './theme/styles/index.css';

/** Props, narrowed to what a widget mounted INSIDE this app can vary. */
interface EliteaAssistantProps {
  readonly title?: string;
  readonly placeholder?: string;
  readonly welcomeMessage?: string;
  readonly position?: 'bottom-right' | 'bottom-left';
  readonly theme?: 'light' | 'dark';
  readonly colors?: TEliteaAssistantColors;
  /** The page context sent with every question — see `lib/useAssistantContext`. */
  readonly supportAssistantContext?: TSupportAssistantContext | undefined;
  /** The resolved config, already fetched by the mount gate. */
  readonly config: TAssistantConfig;
}

const EliteaAssistant = forwardRef<TEliteaAssistantRef, EliteaAssistantProps>(
  (props, ref) => {
    const {
      title: titleProp = `${productName()} Assistant`,
      placeholder: placeholderProp = 'Type a message...',
      welcomeMessage:
        welcomeMessageProp = `Hi! I'm your ${productName()} Support Assistant.\nAsk me anything about ${productName()} or report any issues you're experiencing. I have context about your current screen and settings.`,
      position = 'bottom-right',
      theme = 'light',
      colors,
      supportAssistantContext,
      config,
    } = props;

    const cssVars = useMemo(
      () => colorsToCSSProperties(resolveColors(theme, colors)),
      [theme, colors],
    );

    // Built once for the widget's lifetime. A new adapter identity on every render
    // would re-run `useInitAssistant`'s effect — which is keyed on `api` — and
    // refetch the config and the whole history list on each parent render.
    const api = useMemo(() => createSupportApi(), []);

    const {
      title,
      welcomeMessage,
      placeholder,
      supportProjectId,
      user,
      history: initialHistory,
      lastConversation,
      isLoading: isInitLoading,
    } = useInitAssistant({
      api,
      config,
      title: titleProp,
      welcomeMessage: welcomeMessageProp,
      placeholder: placeholderProp,
    });

    const {
      isOpen,
      isExpanded,
      open,
      close,
      toggle,
      expandFullscreen,
      collapseFullscreen,
      toggleFullscreen,
    } = useAssistantState();

    const {
      popupVissible,
      showPopup: showPopupBase,
      hidePopup: hidePopupBase,
      popupText,
    } = usePopup(isOpen);

    /*
     * The popup no longer captures a screenshot alongside itself — see
     * `chat.hook.ts` for why `html-to-image` was not brought along. `showPopup`
     * and `hidePopup` keep their names and their place on the imperative handle,
     * because what they are FOR (a proactive nudge from the app) is unchanged.
     */

    /*
     * THE HANDLE IS BUILT ONCE.
     *
     * The reference rebuilds it whenever `isOpen` or `isExpanded` changes, which
     * makes a ten-entry dependency array — over this app's budget of eight — and,
     * more to the point, hands every holder of the ref a NEW object on every
     * open and close. A consumer that captured `ref.current`, which is exactly
     * what an imperative handle invites, would then be holding a stale one.
     *
     * Every method except the two READERS is already stable: `useAssistantState`
     * memoises all six with empty dependency arrays, and `usePopup` does the
     * same. So only `isOpen()` and `isExpanded()` need the live value, and they
     * take it from a ref updated on each render rather than from a closure that
     * has to be rebuilt to see it.
     */
    const stateRef = useRef({ isOpen, isExpanded });
    stateRef.current = { isOpen, isExpanded };

    const controlsRef = useRef({
      open,
      close,
      toggle,
      expandFullscreen,
      collapseFullscreen,
      toggleFullscreen,
      showPopup: showPopupBase,
      hidePopup: hidePopupBase,
    });
    controlsRef.current = {
      open,
      close,
      toggle,
      expandFullscreen,
      collapseFullscreen,
      toggleFullscreen,
      showPopup: showPopupBase,
      hidePopup: hidePopupBase,
    };

    useImperativeHandle(
      ref,
      () => ({
        open: () => controlsRef.current.open(),
        close: () => controlsRef.current.close(),
        toggle: () => controlsRef.current.toggle(),
        expandFullscreen: () => controlsRef.current.expandFullscreen(),
        collapseFullscreen: () => controlsRef.current.collapseFullscreen(),
        toggleFullscreen: () => controlsRef.current.toggleFullscreen(),
        showPopup: () => controlsRef.current.showPopup(),
        hidePopup: () => controlsRef.current.hidePopup(),
        isOpen: () => stateRef.current.isOpen,
        isExpanded: () => stateRef.current.isExpanded,
      }),
      [],
    );

    return (
      <ApiContext.Provider value={api}>
        <SupportAssistantContextValue.Provider
          value={supportAssistantContext ?? null}
        >
          <ThemeContext.Provider value={theme}>
            <div
              className={`elitea-assistant-container elitea-assistant-container--${position}`}
              style={cssVars as React.CSSProperties}
            >
              <ChatWindow
                title={title}
                placeholder={placeholder}
                welcomeMessage={welcomeMessage}
                avatar={user.avatar}
                supportProjectId={supportProjectId}
                initialHistory={initialHistory}
                lastConversation={lastConversation}
                isInitLoading={isInitLoading}
                isOpen={isOpen}
                onClose={close}
                expanded={isExpanded}
                onExpand={toggleFullscreen}
              />
              {popupVissible && !isOpen && (
                <PopupMessage message={popupText} onClose={hidePopupBase} />
              )}
              <ChatButton onClick={toggle} />
            </div>
          </ThemeContext.Provider>
        </SupportAssistantContextValue.Provider>
      </ApiContext.Provider>
    );
  },
);

EliteaAssistant.displayName = 'EliteaAssistant';

export { EliteaAssistant };
