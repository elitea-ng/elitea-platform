//! One image-owned client for the original Rust Code process.
#[path = "platform_client.rs"]
mod platform_client;
#[path = "platform_pipe.rs"]
mod platform_pipe;
pub use platform_client::{Operation, PlatformError};
use platform_client::SandboxClient;
use platform_pipe::RetainedPipeExchange;
use std::{io::{Stdin, Stdout}, sync::{Mutex, OnceLock}};

type Client = SandboxClient<RetainedPipeExchange<Stdin, Stdout>>;
static CLIENT: OnceLock<Mutex<Client>> = OnceLock::new();

pub(crate) fn initialize(maximum: u64) -> Result<(), PlatformError> {
    let client = SandboxClient::new(RetainedPipeExchange::new(std::io::stdin(), std::io::stdout()), maximum)?;
    CLIENT.set(Mutex::new(client)).map_err(|_| PlatformError::InvalidResource)
}

/// The compiler fixes these modules. User code cannot select a path or transport endpoint.
pub fn with_client<T>(operation: impl FnOnce(&mut Client) -> Result<T, PlatformError>) -> Result<T, PlatformError> {
    let client = CLIENT.get().ok_or(PlatformError::AuthorizationDenied)?;
    let mut owner = client.try_lock().map_err(|_| PlatformError::UnknownEffect)?;
    operation(&mut owner)
}
