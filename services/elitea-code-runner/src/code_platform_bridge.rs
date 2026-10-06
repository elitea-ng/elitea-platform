//! A fixed image parent retains the original child and bridges bounded pipes.
use super::{
    code_platform_mailbox::{Mailbox, MailboxIdentity},
    code_platform_signature::{self, ExpectedReply, TrustedKeys},
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{io, time::Duration};
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWriteExt},
    process::Command,
};

const MAX_REQUEST: usize = 8 + 262_144 + 65_536;
const MAX_REPLY: usize = 8 + 4096 + 8 + 2_097_152 + 65_536;
const RESULT_LIMIT: usize = 256 * 1024;
fn refused() -> io::Error {
    io::Error::other("Code platform retained bridge refused")
}

/// This value is constructed from verified original Supervisor launch metadata.
/// It is retained in the parent; Code cannot select or deserialize this authority.
pub(crate) struct LaunchBinding {
    runtime_id: String,
    prepared: [u8; 32],
    policy: [u8; 32],
    max_calls: u64,
    max_total_bytes: u64,
    timeout: Duration,
    keys: TrustedKeys,
}
impl LaunchBinding {
    pub(crate) fn new(
        runtime_id: String,
        prepared: [u8; 32],
        policy: [u8; 32],
        max_calls: u64,
        max_total_bytes: u64,
        timeout: Duration,
        keys: TrustedKeys,
    ) -> io::Result<Self> {
        if runtime_id.is_empty()
            || runtime_id.len() > 512
            || runtime_id.contains(['\0', '\r', '\n'])
            || prepared == [0; 32]
            || policy == [0; 32]
            || !(1..=4096).contains(&max_calls)
            || !(1..=64 * 1024 * 1024).contains(&max_total_bytes)
            || timeout.as_secs() < 1
            || timeout.as_secs() > 3600
        {
            return Err(refused());
        }
        Ok(Self {
            runtime_id,
            prepared,
            policy,
            max_calls,
            max_total_bytes,
            timeout,
            keys,
        })
    }
    fn identity(&self) -> io::Result<MailboxIdentity> {
        MailboxIdentity::from_verified_runtime(self.runtime_id.clone(), self.prepared)
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RequestHeader {
    revision: u8,
    sequence: u64,
    operation: String,
    resource: serde_json::Value,
    arguments: serde_json::Value,
}
async fn read_frame<R: AsyncRead + Unpin>(reader: &mut R) -> io::Result<Option<Vec<u8>>> {
    let mut lengths = [0; 8];
    let first = reader.read(&mut lengths[..1]).await?;
    if first == 0 {
        return Ok(None);
    }
    reader.read_exact(&mut lengths[1..]).await?;
    let header = u32::from_be_bytes(lengths[..4].try_into().map_err(|_| refused())?) as usize;
    let body = u32::from_be_bytes(lengths[4..].try_into().map_err(|_| refused())?) as usize;
    if header == 0 || header > 262_144 || body > 65_536 {
        return Err(refused());
    }
    let mut frame = Vec::with_capacity(8 + header + body);
    frame.extend_from_slice(&lengths);
    frame.resize(8 + header + body, 0);
    reader.read_exact(&mut frame[8..]).await?;
    Ok(Some(frame))
}
fn request_sequence(frame: &[u8], next: u64) -> io::Result<u64> {
    if frame.len() < 8 || frame.len() > MAX_REQUEST {
        return Err(refused());
    }
    let header = u32::from_be_bytes(frame[..4].try_into().map_err(|_| refused())?) as usize;
    if header > 262_144 || frame.len() < 8 + header {
        return Err(refused());
    }
    let request: RequestHeader =
        serde_json::from_slice(&frame[8..8 + header]).map_err(|_| refused())?;
    if request.revision != 1
        || request.sequence != next
        || request.operation.len() > 64
        || !request.resource.is_object()
        || !request.arguments.is_object()
    {
        return Err(refused());
    }
    Ok(request.sequence)
}

/// The outer runner still owns CPU, logs, PIDs and the process deadline.
/// A bridge observation failure leaves the original mailbox intact for recovery.
pub(crate) async fn run(mut command: Command, binding: LaunchBinding) -> io::Result<()> {
    let identity = binding.identity()?;
    let mailbox = Mailbox::open(binding.identity()?)?;
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::inherit())
        .kill_on_drop(true);
    let mut child = command.spawn()?;
    let mut stdin = child.stdin.take().ok_or_else(refused)?;
    let mut stdout = child.stdout.take().ok_or_else(refused)?;
    let deadline = tokio::time::Instant::now() + binding.timeout;
    let mut sequence = 0;
    let mut total = 0_u64;
    let mut polls = 0_u64;
    let poll_limit = binding
        .timeout
        .as_secs()
        .saturating_mul(4)
        .saturating_add(1);
    let result = async {
        loop {
            let frame = tokio::time::timeout_at(deadline, read_frame(&mut stdout))
                .await
                .map_err(|_| refused())??;
            let Some(frame) = frame else {
                break;
            };
            if sequence >= binding.max_calls
                || frame.len() as u64 > binding.max_total_bytes.saturating_sub(total)
            {
                return Err(refused());
            }
            let next = request_sequence(&frame, sequence + 1)?;
            let request_sha: [u8; 32] = Sha256::digest(&frame).into();
            // Exact create/rename publication happens once. No resend loop exists.
            mailbox.publish_request(&identity, &frame)?;
            total += frame.len() as u64;
            let reply = loop {
                if let Some(reply) = mailbox.read_reply(&identity)? {
                    break reply;
                }
                if polls >= poll_limit || tokio::time::Instant::now() >= deadline {
                    return Err(refused());
                }
                polls += 1;
                tokio::select! {
                    status=child.wait()=>{let _=status?;return Err(refused());},
                    ()=tokio::time::sleep(Duration::from_millis(250))=>{}
                }
            };
            if reply.len() > MAX_REPLY {
                return Err(refused());
            }
            let verified = code_platform_signature::verify(
                &reply,
                &ExpectedReply {
                    runtime_id: &binding.runtime_id,
                    prepared: &binding.prepared,
                    policy: &binding.policy,
                    sequence: next,
                    request_sha256: &request_sha,
                },
                &binding.keys,
            )?;
            if verified.exact_frame.len() as u64 > binding.max_total_bytes.saturating_sub(total) {
                return Err(refused());
            }
            // Signature verification precedes every byte delivered to the child.
            tokio::time::timeout_at(deadline, async {
                stdin.write_all(verified.exact_frame).await?;
                stdin.flush().await
            })
            .await
            .map_err(|_| refused())??;
            total += verified.exact_frame.len() as u64;
            sequence = next;
            mailbox.retire(&identity)?;
        }
        let status = tokio::time::timeout_at(deadline, child.wait())
            .await
            .map_err(|_| refused())??;
        if !status.success() {
            return Err(refused());
        }
        let result = read_result()?;
        tokio::io::stdout().write_all(&result).await?;
        tokio::io::stdout().flush().await?;
        Ok(())
    }
    .await;
    if result.is_err() {
        let _ = tokio::time::timeout(Duration::from_secs(2), child.kill()).await;
    }
    result
}

fn read_result() -> io::Result<Vec<u8>> {
    use std::io::Read as _;
    let fd = rustix::fs::open(
        "/workspace/.elitea-code-result",
        rustix::fs::OFlags::RDONLY
            | rustix::fs::OFlags::NOFOLLOW
            | rustix::fs::OFlags::NONBLOCK
            | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )?;
    let file = std::fs::File::from(fd);
    let stat = file.metadata()?;
    if !stat.is_file() || stat.len() > RESULT_LIMIT as u64 {
        return Err(refused());
    }
    let mut bytes = Vec::new();
    file.take(RESULT_LIMIT as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > RESULT_LIMIT {
        return Err(refused());
    }
    let result: serde_json::Value = serde_json::from_slice(&bytes).map_err(|_| refused())?;
    if result
        .as_object()
        .is_none_or(|v| v.len() != 2 || !v.contains_key("result"))
        || result["revision"] != 1
    {
        return Err(refused());
    }
    Ok(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn length_limits_precede_allocation_and_partial_frames_fail() {
        for bytes in [
            vec![0, 0, 0, 1, 0, 0, 0, 0],
            vec![0xff; 8],
            vec![1],
            vec![0; 7],
        ] {
            let mut input = bytes.as_slice();
            assert!(read_frame(&mut input).await.is_err());
        }
        let mut empty = &[][..];
        assert!(read_frame(&mut empty).await.unwrap().is_none());
    }
    #[test]
    fn launch_rejects_invalid_limits_and_replacement_identity() {
        let keys = || TrustedKeys::new(vec![("main-v1".into(), [1; 32])]).unwrap();
        assert!(
            LaunchBinding::new(
                "pod".into(),
                [1; 32],
                [2; 32],
                0,
                1024,
                Duration::from_secs(1),
                keys()
            )
            .is_err()
        );
        assert!(
            LaunchBinding::new(
                "pod".into(),
                [1; 32],
                [2; 32],
                1,
                0,
                Duration::from_secs(1),
                keys()
            )
            .is_err()
        );
        assert!(
            LaunchBinding::new(
                "pod".into(),
                [1; 32],
                [2; 32],
                1,
                1024,
                Duration::from_secs(0),
                keys()
            )
            .is_err()
        );
        let original = LaunchBinding::new(
            "original".into(),
            [1; 32],
            [2; 32],
            1,
            1024,
            Duration::from_secs(1),
            keys(),
        )
        .unwrap();
        let replacement = LaunchBinding::new(
            "replacement".into(),
            [1; 32],
            [2; 32],
            1,
            1024,
            Duration::from_secs(1),
            keys(),
        )
        .unwrap();
        assert!(
            !original
                .identity()
                .unwrap()
                .matches(&replacement.identity().unwrap())
        );
    }
}
