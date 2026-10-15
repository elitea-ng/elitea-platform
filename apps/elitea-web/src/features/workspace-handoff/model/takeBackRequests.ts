/**
 * A "take back requested" notification (kind `workspace_reclaim_requested`,
 * raised by the web's Take back button) asks the desktop to start a take
 * back of one thread. The toast that shows it lives in the shell; the dialog
 * that does the work lives on the thread's page. This store is the hand-off
 * between the two: the page opens the take back for `requested` when it is
 * its own thread, and clears it.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

interface TakeBackRequestsState {
  /** The conversation (its UUID or id, lower case) the person asked to take back, until its page picks it up. */
  readonly requested: string | null;
  readonly request: (conversation: string) => void;
  readonly clear: (conversation: string) => void;
}

type TakeBackRequestsStore = UseBoundStore<StoreApi<TakeBackRequestsState>>;

function createTakeBackRequestsStore(): TakeBackRequestsStore {
  return create<TakeBackRequestsState>((set) => ({
    requested: null,
    request: (conversation) => set({ requested: conversation.toLowerCase() }),
    clear: (conversation) => set((state) => (state.requested === conversation.toLowerCase() ? { requested: null } : state)),
  }));
}

let instance: TakeBackRequestsStore | undefined;

function resolveStore(): TakeBackRequestsStore {
  instance ??= createTakeBackRequestsStore();
  return instance;
}

function useTakeBackRequestsHook<T>(selector: (state: TakeBackRequestsState) => T): T {
  return resolveStore()(selector);
}

/** The lazily-constructed singleton, with the zustand `getState` surface. */
export const useTakeBackRequests = Object.assign(useTakeBackRequestsHook, {
  getState: (): TakeBackRequestsState => resolveStore().getState(),
});
