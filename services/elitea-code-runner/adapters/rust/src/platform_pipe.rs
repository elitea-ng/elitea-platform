//! The broker-enabled image launch owns these pipes. No user path or argv is accepted.
use std::io::{Read, Write};
use super::platform_client::{Exchange, PlatformError};

pub struct RetainedPipeExchange<R, W> { reader: R, writer: W, unknown: bool }
impl<R: Read, W: Write> RetainedPipeExchange<R, W> {
    pub fn new(reader: R, writer: W) -> Self { Self { reader, writer, unknown: false } }
}
impl<R: Read, W: Write> Exchange for RetainedPipeExchange<R, W> {
    fn exchange(&mut self, frame: &[u8]) -> Result<Vec<u8>, PlatformError> {
        if self.unknown || frame.len() < 8 || frame.len() > 8 + 262_144 + 65_536 {
            return Err(PlatformError::UnknownEffect);
        }
        let observed = (|| {
            self.writer.write_all(frame).map_err(|_| PlatformError::UnknownEffect)?;
            self.writer.flush().map_err(|_| PlatformError::UnknownEffect)?;
            let mut prefix = [0; 8];
            self.reader.read_exact(&mut prefix).map_err(|_| PlatformError::UnknownEffect)?;
            let head = u32::from_be_bytes(prefix[..4].try_into().map_err(|_| PlatformError::InvalidFrame)?) as usize;
            let body = u32::from_be_bytes(prefix[4..].try_into().map_err(|_| PlatformError::InvalidFrame)?) as usize;
            if head > 2_097_152 || body > 65_536 { return Err(PlatformError::UnknownEffect); }
            let mut output = vec![0; 8 + head + body];
            output[..8].copy_from_slice(&prefix);
            self.reader.read_exact(&mut output[8..]).map_err(|_| PlatformError::UnknownEffect)?;
            Ok(output)
        })();
        if observed.is_err() { self.unknown = true; }
        observed
    }
}
