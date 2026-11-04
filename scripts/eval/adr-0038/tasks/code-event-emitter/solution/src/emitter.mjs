// Minimal synchronous event emitter.
//
// Each registration is a record { fn, once }. Records are compared by
// identity, so an unsubscribe closure removes exactly its own registration.

export class Emitter {
  constructor() {
    this.listeners = new Map();
  }

  _list(event) {
    return this.listeners.get(event) ?? [];
  }

  _add(event, fn, once) {
    if (typeof fn !== 'function') throw new TypeError('listener must be a function');
    const record = { fn, once };
    this.listeners.set(event, [...this._list(event), record]);
    return () => this._removeRecord(event, record);
  }

  _removeRecord(event, record) {
    const list = this._list(event);
    const i = list.indexOf(record);
    if (i === -1) return false;
    // Copy-on-write: an in-flight emit keeps iterating its own snapshot.
    const next = list.slice(0, i).concat(list.slice(i + 1));
    if (next.length) this.listeners.set(event, next);
    else this.listeners.delete(event);
    return true;
  }

  on(event, fn) {
    return this._add(event, fn, false);
  }

  once(event, fn) {
    return this._add(event, fn, true);
  }

  off(event, fn) {
    const list = this._list(event);
    for (let i = list.length - 1; i >= 0; i--) {
      if (list[i].fn === fn) {
        this._removeRecord(event, list[i]);
        break;
      }
    }
    return this;
  }

  emit(event, ...args) {
    const snapshot = this._list(event);
    if (snapshot.length === 0) {
      if (event === 'error') {
        const err = args[0];
        throw err instanceof Error ? err : new Error(`Unhandled error event: ${String(err)}`);
      }
      return false;
    }
    for (const record of snapshot) {
      if (record.once) {
        // Mark and remove before calling so a re-entrant emit cannot run it
        // again; skip it if a nested emit already consumed it.
        if (record.fired) continue;
        record.fired = true;
        this._removeRecord(event, record);
      }
      record.fn.apply(this, args);
    }
    return true;
  }

  listenerCount(event) {
    return this._list(event).length;
  }
}
