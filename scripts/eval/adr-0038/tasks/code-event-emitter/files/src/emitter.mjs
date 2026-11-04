// Minimal synchronous event emitter.

export class Emitter {
  constructor() {
    this.listeners = new Map();
  }

  _list(event) {
    if (!this.listeners.has(event)) this.listeners.set(event, []);
    return this.listeners.get(event);
  }

  on(event, fn) {
    this._list(event).push(fn);
    return () => this.off(event, fn);
  }

  once(event, fn) {
    const wrapper = (...args) => {
      fn(...args);
      this.off(event, wrapper);
    };
    this._list(event).push(wrapper);
    return () => this.off(event, wrapper);
  }

  off(event, fn) {
    const list = this._list(event);
    const i = list.indexOf(fn);
    if (i !== -1) list.splice(i, 1);
    return this;
  }

  emit(event, ...args) {
    const list = this._list(event);
    for (let i = 0; i < list.length; i++) {
      list[i](...args);
    }
    return list.length > 0;
  }

  listenerCount(event) {
    return this._list(event).length;
  }
}
