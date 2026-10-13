//! Per-thread counters of the expensive deck operations, for tests that
//! assert a tool does not serialise or parse more than it must. A call counts
//! on the thread that makes it (the library work runs on a guarded thread of
//! its own, so a global counter would also count other tests' work).

use std::cell::Cell;

#[derive(Clone, Copy)]
pub(crate) enum Op {
    Open,
    Save,
    PackageRead,
}

thread_local! {
    static COUNTS: Cell<[u64; 3]> = const { Cell::new([0; 3]) };
}

pub(crate) fn bump(op: Op) {
    COUNTS.with(|c| {
        let mut v = c.get();
        v[op as usize] += 1;
        c.set(v);
    });
}

/// `(opens, saves, package_reads)` made on this thread since the last reset.
pub(crate) fn read() -> (u64, u64, u64) {
    let v = COUNTS.with(Cell::get);
    (v[0], v[1], v[2])
}

pub(crate) fn reset() {
    COUNTS.with(|c| c.set([0; 3]));
}
