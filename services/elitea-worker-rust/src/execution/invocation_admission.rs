//! Bounded admission reserved before the durable `BeginExecution` transition.
//!
//! A reservation covers running plus queued native ADK invocations. It is
//! acquired before Main records execution start, retained through input and
//! authorization, and moved into the execution coordinator at the actual
//! submission boundary. Dropping an unused reservation returns capacity.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::time::timeout;

const MAX_INVOCATION_CAPACITY: usize = 4_096;
const MAX_ADMISSION_TIMEOUT: Duration = Duration::from_mins(5);

/// Immutable running-plus-queued invocation limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvocationAdmissionConfig {
    capacity: usize,
    wait_timeout: Duration,
}

impl InvocationAdmissionConfig {
    /// Validate bounded admission policy before the worker begins intake.
    ///
    /// # Errors
    ///
    /// Rejects zero/excessive capacity or a zero/excessive wait deadline.
    pub fn new(capacity: usize, wait_timeout: Duration) -> Result<Self, InvocationAdmissionError> {
        if capacity == 0
            || capacity > MAX_INVOCATION_CAPACITY
            || wait_timeout.is_zero()
            || wait_timeout > MAX_ADMISSION_TIMEOUT
        {
            return Err(InvocationAdmissionError::InvalidConfiguration(
                "the invocation admission limits are outside the approved range",
            ));
        }
        Ok(Self {
            capacity,
            wait_timeout,
        })
    }

    #[must_use]
    pub const fn capacity(self) -> usize {
        self.capacity
    }

    #[must_use]
    pub const fn wait_timeout(self) -> Duration {
        self.wait_timeout
    }
}

/// Stable categories suitable for metrics and no-ACK routing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InvocationAdmissionErrorCode {
    InvalidConfiguration,
    Saturated,
    Draining,
}

impl InvocationAdmissionErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "invocation_admission_invalid_configuration",
            Self::Saturated => "invocation_admission_saturated",
            Self::Draining => "invocation_admission_draining",
        }
    }
}

/// Safe, data-free admission failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InvocationAdmissionError {
    InvalidConfiguration(&'static str),
    Saturated(&'static str),
    Draining(&'static str),
}

impl InvocationAdmissionError {
    #[must_use]
    pub const fn code(self) -> InvocationAdmissionErrorCode {
        match self {
            Self::InvalidConfiguration(_) => InvocationAdmissionErrorCode::InvalidConfiguration,
            Self::Saturated(_) => InvocationAdmissionErrorCode::Saturated,
            Self::Draining(_) => InvocationAdmissionErrorCode::Draining,
        }
    }

    /// Every runtime admission failure leaves the bus command untouched and
    /// is eligible for another worker; invalid process configuration is not.
    #[must_use]
    pub const fn retryable(self) -> bool {
        matches!(self, Self::Saturated(_) | Self::Draining(_))
    }
}

impl fmt::Display for InvocationAdmissionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::Saturated(message)
            | Self::Draining(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for InvocationAdmissionError {}

struct AdmissionState {
    capacity: Arc<Semaphore>,
    accepting: AtomicBool,
    wait_timeout: Duration,
    configured_capacity: usize,
}

/// Cloneable handle for one process-wide bounded invocation admission pool.
#[derive(Clone)]
pub struct InvocationAdmission {
    state: Arc<AdmissionState>,
}

impl InvocationAdmission {
    #[must_use]
    pub fn new(config: InvocationAdmissionConfig) -> Self {
        Self {
            state: Arc::new(AdmissionState {
                capacity: Arc::new(Semaphore::new(config.capacity)),
                accepting: AtomicBool::new(true),
                wait_timeout: config.wait_timeout,
                configured_capacity: config.capacity,
            }),
        }
    }

    /// Reserve one running-or-queued slot before `BeginExecution`.
    ///
    /// The operation is cancellation-safe: cancelling a wait withdraws it and
    /// does not consume capacity. A concurrent drain is checked before and
    /// after acquisition, so no new reservation escapes once admission stops.
    ///
    /// # Errors
    ///
    /// Returns retryable saturation or draining without granting authority.
    pub async fn reserve(&self) -> Result<InvocationReservation, InvocationAdmissionError> {
        if !self.state.accepting.load(Ordering::Acquire) {
            return Err(draining());
        }
        let acquisition = Arc::clone(&self.state.capacity).acquire_owned();
        let permit = timeout(self.state.wait_timeout, acquisition)
            .await
            .map_err(|_| saturated())?
            .map_err(|_| draining())?;
        if !self.state.accepting.load(Ordering::Acquire) {
            drop(permit);
            return Err(draining());
        }
        Ok(InvocationReservation {
            _permit: permit,
            owner: Arc::clone(&self.state),
        })
    }

    /// Stop new admission and wake every waiter. Existing reservations remain
    /// owned until their execution coordinator drops them.
    pub fn stop(&self) {
        if self.state.accepting.swap(false, Ordering::AcqRel) {
            self.state.capacity.close();
        }
    }

    #[must_use]
    pub fn is_accepting(&self) -> bool {
        self.state.accepting.load(Ordering::Acquire)
    }

    #[must_use]
    pub fn available_capacity(&self) -> usize {
        self.state.capacity.available_permits()
    }

    #[must_use]
    pub fn configured_capacity(&self) -> usize {
        self.state.configured_capacity
    }

    pub(crate) fn owns(&self, reservation: &InvocationReservation) -> bool {
        Arc::ptr_eq(&self.state, &reservation.owner)
    }
}

/// One non-cloneable slot reserved for a future native ADK invocation.
///
/// The type intentionally exposes no raw semaphore permit. The fresh execution
/// coordinator will consume and retain it through the underlying invocation,
/// including any caller-cancellation wait for real completion.
pub struct InvocationReservation {
    _permit: OwnedSemaphorePermit,
    owner: Arc<AdmissionState>,
}

fn saturated() -> InvocationAdmissionError {
    InvocationAdmissionError::Saturated(
        "the bounded invocation queue remained full until its admission deadline",
    )
}

fn draining() -> InvocationAdmissionError {
    InvocationAdmissionError::Draining("the worker is no longer accepting agent invocations")
}
