//! Image-owned result wrapper. User output remains untrusted.
mod user;
use std::io::{Read, Write};
struct Bounded(Vec<u8>);
impl Write for Bounded {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if self.0.len() + bytes.len() > 256 * 1024 {
            return Err(std::io::Error::other("Code result exceeds 256 KiB"));
        }
        self.0.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut bytes = Vec::new();
    std::fs::File::open("/workspace/rust-job/input.json")?
        .take(1024 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 1024 * 1024 {
        return Err("Code input exceeds its limit".into());
    }
    let input = serde_json::from_slice(&bytes)?;
    let result = user::run(input)?;
    let mut output = Bounded(Vec::new());
    serde_json::to_writer(&mut output, &result)?;
    std::fs::write("/workspace/rust-job/result.json", output.0)?;
    Ok(())
}
