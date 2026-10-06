//! Single-owner replacement for the restricted Redis command transport.
//!
//! A Redis operation is attempted exactly once on one connection generation.
//! A retryable failure removes that generation, but never replays the ambiguous
//! command. The outer serve loop owns backoff and explicitly establishes the
//! next generation before issuing new work.

#![allow(dead_code)] // The capability-disabled serve loop is the first consumer.

use std::{future::Future, pin::Pin, sync::Arc};

use async_trait::async_trait;
use tokio::sync::Mutex;

use super::redis_commands::{
    RedisCommandDelivery, RedisRetirementClient, RedisRetirementClientError,
    RedisRetirementRequest, RedisRetirementResponse,
};
use super::redis_streams::{
    RedisOwnedPendingPage, RedisReclaimPage, RedisStreamsClient, RedisStreamsError,
};

pub(crate) type RedisGenerationFuture<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// Restricted operations implemented by one authenticated Redis generation.
///
/// The trait is crate-private so test doubles cannot widen the production
/// Redis surface. Every future owns an [`Arc`] to the exact generation and may
/// therefore outlive a caller that stops waiting for it.
pub(crate) trait RedisStreamsConnection:
    RedisRetirementClient + Send + Sync + 'static
{
    fn delivery_batch_size(&self) -> u64;

    fn read_new(
        self: Arc<Self>,
        count: u64,
        block_millis: u64,
    ) -> RedisGenerationFuture<Result<Vec<RedisCommandDelivery>, RedisStreamsError>>;

    fn read_owned_pending(
        self: Arc<Self>,
        count: u64,
        start_id: String,
    ) -> RedisGenerationFuture<Result<RedisOwnedPendingPage, RedisStreamsError>>;

    fn reclaim_page(
        self: Arc<Self>,
        min_idle_millis: u64,
        start_id: String,
        count: u64,
    ) -> RedisGenerationFuture<Result<RedisReclaimPage, RedisStreamsError>>;

    fn heartbeat_owned_pending(
        self: Arc<Self>,
        entry_ids: Vec<String>,
    ) -> RedisGenerationFuture<Result<Vec<String>, RedisStreamsError>>;

    fn close(self: Arc<Self>) -> RedisGenerationFuture<Result<(), RedisStreamsError>>;
}

impl RedisStreamsConnection for RedisStreamsClient {
    fn delivery_batch_size(&self) -> u64 {
        self.delivery_batch_size()
    }

    fn read_new(
        self: Arc<Self>,
        count: u64,
        block_millis: u64,
    ) -> RedisGenerationFuture<Result<Vec<RedisCommandDelivery>, RedisStreamsError>> {
        Box::pin(
            async move { RedisStreamsClient::read_new(self.as_ref(), count, block_millis).await },
        )
    }

    fn read_owned_pending(
        self: Arc<Self>,
        count: u64,
        start_id: String,
    ) -> RedisGenerationFuture<Result<RedisOwnedPendingPage, RedisStreamsError>> {
        Box::pin(async move {
            RedisStreamsClient::read_owned_pending(self.as_ref(), count, &start_id).await
        })
    }

    fn reclaim_page(
        self: Arc<Self>,
        min_idle_millis: u64,
        start_id: String,
        count: u64,
    ) -> RedisGenerationFuture<Result<RedisReclaimPage, RedisStreamsError>> {
        Box::pin(async move {
            RedisStreamsClient::reclaim_page(self.as_ref(), min_idle_millis, &start_id, count).await
        })
    }

    fn heartbeat_owned_pending(
        self: Arc<Self>,
        entry_ids: Vec<String>,
    ) -> RedisGenerationFuture<Result<Vec<String>, RedisStreamsError>> {
        Box::pin(async move {
            RedisStreamsClient::heartbeat_owned_pending(self.as_ref(), &entry_ids).await
        })
    }

    fn close(self: Arc<Self>) -> RedisGenerationFuture<Result<(), RedisStreamsError>> {
        Box::pin(async move { RedisStreamsClient::close(self.as_ref()).await })
    }
}

/// Reconstruct one fresh authenticated Redis generation.
///
/// Production composition must reload bounded, zeroizing workload TLS material
/// for every call. Implementations must be cancellation-safe: dropping the
/// returned future cannot leave a detached connected client.
pub(crate) trait RedisStreamsConnector: Send + Sync + 'static {
    type Connection: RedisStreamsConnection;

    fn connect(
        self: Arc<Self>,
    ) -> RedisGenerationFuture<Result<Arc<Self::Connection>, RedisStreamsError>>;
}

struct ConnectedGeneration<T> {
    number: u64,
    connection: Arc<T>,
}

struct RedisGenerationState<T> {
    current: Option<ConnectedGeneration<T>>,
    next_number: u64,
    closed: bool,
}

/// Replaceable restricted Redis owner shared by intake, heartbeat and exact
/// post-settlement retirement.
///
/// Connection establishment is serialized under one async mutex, while normal
/// Redis operations clone the current generation and release that mutex before
/// awaiting network I/O. A retryable failure invalidates only the generation
/// on which it occurred; a late old-generation failure cannot evict a newer
/// connection.
pub(crate) struct RedisStreamsHandle<C>
where
    C: RedisStreamsConnector,
{
    connector: Arc<C>,
    state: Mutex<RedisGenerationState<C::Connection>>,
}

impl<C> RedisStreamsHandle<C>
where
    C: RedisStreamsConnector,
{
    pub(crate) fn new(connector: Arc<C>) -> Self {
        Self {
            connector,
            state: Mutex::new(RedisGenerationState {
                current: None,
                next_number: 1,
                closed: false,
            }),
        }
    }

    /// Establish exactly one generation or return the already-current one.
    ///
    /// The mutex deliberately remains held across connection establishment so
    /// simultaneous failures cannot create parallel replacement generations.
    /// Cancelling the caller drops both the connector future and the mutex
    /// guard; a later caller may retry from the unchanged disconnected state.
    pub(crate) async fn connect(&self) -> Result<u64, RedisStreamsError> {
        let mut state = self.state.lock().await;
        if state.closed {
            return Err(RedisStreamsError::closed());
        }
        if let Some(current) = state.current.as_ref() {
            return Ok(current.number);
        }
        let connection = Arc::clone(&self.connector).connect().await?;
        let number = state.next_number;
        state.next_number = state.next_number.checked_add(1).ok_or_else(|| {
            RedisStreamsError::resource_exhausted("the Redis generation counter is exhausted")
        })?;
        state.current = Some(ConnectedGeneration { number, connection });
        Ok(number)
    }

    pub(crate) async fn delivery_batch_size(&self) -> Result<u64, RedisStreamsError> {
        let current = self.current().await?;
        Ok(current.connection.delivery_batch_size())
    }

    pub(crate) async fn read_new(
        &self,
        count: u64,
        block_millis: u64,
    ) -> Result<Vec<RedisCommandDelivery>, RedisStreamsError> {
        let current = self.current().await?;
        let result = Arc::clone(&current.connection)
            .read_new(count, block_millis)
            .await;
        self.invalidate_retryable(current.number, result.as_ref().err())
            .await;
        result
    }

    pub(crate) async fn read_owned_pending(
        &self,
        count: u64,
        start_id: String,
    ) -> Result<RedisOwnedPendingPage, RedisStreamsError> {
        let current = self.current().await?;
        let result = Arc::clone(&current.connection)
            .read_owned_pending(count, start_id)
            .await;
        self.invalidate_retryable(current.number, result.as_ref().err())
            .await;
        result
    }

    pub(crate) async fn reclaim_page(
        &self,
        min_idle_millis: u64,
        start_id: String,
        count: u64,
    ) -> Result<RedisReclaimPage, RedisStreamsError> {
        let current = self.current().await?;
        let result = Arc::clone(&current.connection)
            .reclaim_page(min_idle_millis, start_id, count)
            .await;
        self.invalidate_retryable(current.number, result.as_ref().err())
            .await;
        result
    }

    pub(crate) async fn heartbeat_owned_pending(
        &self,
        entry_ids: Vec<String>,
    ) -> Result<Vec<String>, RedisStreamsError> {
        let current = self.current().await?;
        let result = Arc::clone(&current.connection)
            .heartbeat_owned_pending(entry_ids)
            .await;
        self.invalidate_retryable(current.number, result.as_ref().err())
            .await;
        result
    }

    /// Close the current generation and permanently prevent reconnection.
    ///
    /// Normal composition calls this only after delivery and invocation drain.
    /// The closed state is installed before the first await, so cancellation
    /// cannot reopen intake or mint a replacement generation.
    pub(crate) async fn close(&self) -> Result<(), RedisStreamsError> {
        let connection = {
            let mut state = self.state.lock().await;
            if state.closed {
                return Ok(());
            }
            state.closed = true;
            state.current.take().map(|current| current.connection)
        };
        if let Some(connection) = connection {
            connection.close().await?;
        }
        Ok(())
    }

    async fn current(&self) -> Result<ConnectedGeneration<C::Connection>, RedisStreamsError> {
        let state = self.state.lock().await;
        if state.closed {
            return Err(RedisStreamsError::closed());
        }
        let current = state.current.as_ref().ok_or_else(|| {
            RedisStreamsError::unavailable("the Redis command transport requires connection")
        })?;
        Ok(ConnectedGeneration {
            number: current.number,
            connection: Arc::clone(&current.connection),
        })
    }

    async fn invalidate_retryable(&self, generation: u64, error: Option<&RedisStreamsError>) {
        if !error.is_some_and(RedisStreamsError::retryable) {
            return;
        }
        self.invalidate(generation).await;
    }

    async fn invalidate(&self, generation: u64) {
        let mut state = self.state.lock().await;
        if state
            .current
            .as_ref()
            .is_some_and(|current| current.number == generation)
        {
            state.current = None;
        }
    }
}

#[async_trait]
impl<C> RedisRetirementClient for RedisStreamsHandle<C>
where
    C: RedisStreamsConnector,
{
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        let current = self
            .current()
            .await
            .map_err(|error| map_handle_retirement_error(&error))?;
        let result = current.connection.retire_delivery(request).await;
        if result.is_err() {
            self.invalidate(current.number).await;
        }
        result
    }
}

#[async_trait]
impl<C> RedisRetirementClient for Arc<RedisStreamsHandle<C>>
where
    C: RedisStreamsConnector,
{
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        self.as_ref().retire_delivery(request).await
    }
}

fn map_handle_retirement_error(error: &RedisStreamsError) -> RedisRetirementClientError {
    use super::redis_streams::RedisStreamsErrorKind;

    match error.kind() {
        RedisStreamsErrorKind::Authentication => RedisRetirementClientError::Authentication,
        RedisStreamsErrorKind::Timeout => RedisRetirementClientError::Timeout,
        RedisStreamsErrorKind::DependencyUnavailable | RedisStreamsErrorKind::Closed => {
            RedisRetirementClientError::DependencyUnavailable
        }
        RedisStreamsErrorKind::Configuration
        | RedisStreamsErrorKind::ConsumerGroupMissing
        | RedisStreamsErrorKind::Protocol
        | RedisStreamsErrorKind::ResourceExhausted => RedisRetirementClientError::Protocol,
    }
}
