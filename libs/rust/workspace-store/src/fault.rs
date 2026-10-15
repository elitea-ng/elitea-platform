//! adapted from floe (MIT): `floe-store`'s `FaultStore` (a fault-injecting
//! wrapper around a store, with the fault kinds that break "PUT then
//! commit" protocols: a transient error before the operation, the
//! operation applied and its answer lost, a stale read), cut down to
//! scripted, deterministic counts per operation.
//!
//! [`FaultTransport`] wraps any [`StoreTransport`]. Each fault is armed for
//! the next `n` calls of one operation:
//!
//! * [`Fault::Unavailable`]: answer 503 before the operation (nothing
//!   applied);
//! * [`Fault::LostAck`]: apply the operation, then answer as if the network
//!   dropped (the case an idempotent publish exists for);
//! * [`Fault::Dropped`]: answer success without applying (a PUT the object
//!   store acknowledged and lost);
//! * [`Fault::Expired`]: answer 403 (a presigned URL that expired, a stale
//!   plan);
//! * [`Fault::Corrupt`]: apply, then flip a byte of what comes back (a GET).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;

use crate::error::StoreError;
use crate::transport::{
    CompletedPart, FetchPlan, Multipart, PublishRequest, Published, PutTarget, StoreTransport,
    UploadGrant, UploadRequest,
};

/// The operations faults are armed on.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum Op {
    Grant,
    Put,
    Part,
    Complete,
    Publish,
    Plan,
    Get,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    Unavailable,
    LostAck,
    Dropped,
    Expired,
    Corrupt,
}

#[derive(Default, Debug)]
struct Script {
    armed: BTreeMap<Op, Vec<Fault>>,
    calls: BTreeMap<Op, u64>,
    taken: u64,
}

/// A transport whose calls fail as scripted.
pub struct FaultTransport {
    inner: Arc<dyn StoreTransport>,
    script: Mutex<Script>,
}

impl FaultTransport {
    #[must_use]
    pub fn new(inner: Arc<dyn StoreTransport>) -> Self {
        Self {
            inner,
            script: Mutex::new(Script::default()),
        }
    }

    fn script(&self) -> std::sync::MutexGuard<'_, Script> {
        self.script
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Arm `fault` for the next `times` calls of `op` (after any armed
    /// before).
    pub fn arm(&self, op: Op, fault: Fault, times: usize) {
        self.script()
            .armed
            .entry(op)
            .or_default()
            .extend(std::iter::repeat_n(fault, times));
    }

    /// Calls of `op` so far.
    #[must_use]
    pub fn calls(&self, op: Op) -> u64 {
        self.script().calls.get(&op).copied().unwrap_or_default()
    }

    /// Faults taken so far.
    #[must_use]
    pub fn faults_taken(&self) -> u64 {
        self.script().taken
    }

    fn next(&self, op: Op) -> Option<Fault> {
        let mut script = self.script();
        *script.calls.entry(op).or_default() += 1;
        let queue = script.armed.get_mut(&op)?;
        if queue.is_empty() {
            return None;
        }
        let fault = queue.remove(0);
        script.taken += 1;
        Some(fault)
    }
}

fn unavailable() -> StoreError {
    StoreError::http(503, "unavailable", "injected: service unavailable")
}

fn lost() -> StoreError {
    StoreError::Network("injected: the answer was lost".into())
}

fn expired() -> StoreError {
    StoreError::http(403, "expired", "injected: the URL expired")
}

/// Run one call under the fault armed for it.
macro_rules! faulted {
    ($self:ident, $op:expr, $call:expr, $dropped:expr) => {
        match $self.next($op) {
            None => $call.await,
            Some(Fault::Unavailable) => Err(unavailable()),
            Some(Fault::Expired) => Err(expired()),
            Some(Fault::LostAck) => {
                let _ = $call.await;
                Err(lost())
            }
            Some(Fault::Dropped) => $dropped,
            Some(Fault::Corrupt) => $call.await,
        }
    };
}

#[async_trait]
impl StoreTransport for FaultTransport {
    async fn upload_grant(&self, request: &UploadRequest) -> Result<UploadGrant, StoreError> {
        faulted!(
            self,
            Op::Grant,
            self.inner.upload_grant(request),
            Err(lost())
        )
    }

    async fn put(&self, target: &PutTarget, bytes: &[u8]) -> Result<(), StoreError> {
        faulted!(self, Op::Put, self.inner.put(target, bytes), Ok(()))
    }

    async fn put_part(&self, url: &str, bytes: &[u8]) -> Result<String, StoreError> {
        faulted!(
            self,
            Op::Part,
            self.inner.put_part(url, bytes),
            Ok("dropped".into())
        )
    }

    async fn complete_multipart(
        &self,
        multipart: &Multipart,
        parts: &[CompletedPart],
    ) -> Result<(), StoreError> {
        faulted!(
            self,
            Op::Complete,
            self.inner.complete_multipart(multipart, parts),
            Ok(())
        )
    }

    async fn publish(&self, request: &PublishRequest) -> Result<Published, StoreError> {
        faulted!(self, Op::Publish, self.inner.publish(request), Err(lost()))
    }

    async fn fetch_plan(&self, since_seq: u64) -> Result<FetchPlan, StoreError> {
        faulted!(
            self,
            Op::Plan,
            self.inner.fetch_plan(since_seq),
            Err(lost())
        )
    }

    async fn get(&self, url: &str) -> Result<Vec<u8>, StoreError> {
        let fault = self.next(Op::Get);
        match fault {
            None => self.inner.get(url).await,
            Some(Fault::Unavailable) => Err(unavailable()),
            Some(Fault::Expired) => Err(expired()),
            Some(Fault::LostAck | Fault::Dropped) => Err(lost()),
            Some(Fault::Corrupt) => {
                let mut bytes = self.inner.get(url).await?;
                if let Some(last) = bytes.last_mut() {
                    *last ^= 0x01;
                }
                Ok(bytes)
            }
        }
    }
}
