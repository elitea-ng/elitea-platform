//! A git smart-HTTP `receive-pack` client that only CREATES branches: the
//! cloud executor's `git_push` (ADR-0034 decision 10, amendment 2026-10-26).
//!
//! The worker image carries no git binary, and this is narrower than one:
//! two HTTPS requests to the forge's own origin (the transport is pinned to
//! it), the credential in their `Authorization` header and nowhere else (no
//! config file, no credential helper, no environment, no hooks, no
//! `ext::`/`file://`/`ssh` transport), and one command whose old id is
//! zero. git's `receive-pack` refuses a zero old id for a ref that exists,
//! so nothing is ever overwritten, force-pushed or deleted, and the default
//! branch (which exists) can never be the target. A name the remote already
//! has gets the next free `_vN` suffix, read from the advertisement.
//!
//! The pack is whole objects (no deltas). It leaves out what the remote has
//! through the upstream commit the history sits on; a remote that lacks that
//! commit answers `unpack` with an error, which is reported as such.

use std::collections::BTreeSet;
use std::sync::Arc;

use base64::Engine as _;
use elitea_connectors::egress::OriginGuard;
use elitea_connectors::transport::{
    Body, HeaderValue, Method, Request, StatusCode, Transport, Url, header,
};
use zeroize::Zeroizing;

/// The most bytes of a ref advertisement read.
const MAX_ADVERTISEMENT_BYTES: usize = 8 << 20;
/// The most bytes of a push report read.
const MAX_REPORT_BYTES: usize = 256 * 1_024;
/// The most `_vN` suffixes tried.
const MAX_SUFFIX: u32 = 20;
const ZERO_ID: &str = "0000000000000000000000000000000000000000";
const AGENT: &str = "agent=elitea-cloud-executor/1";

/// The HTTPS basic credential for one push. Its `Debug` shows nothing.
pub struct GitCredential {
    header: Zeroizing<String>,
}

impl std::fmt::Debug for GitCredential {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("GitCredential(..)")
    }
}

impl GitCredential {
    #[must_use]
    pub fn basic(user: &str, secret: &str) -> Self {
        let pair = Zeroizing::new(format!("{user}:{secret}"));
        let encoded =
            Zeroizing::new(base64::engine::general_purpose::STANDARD.encode(pair.as_bytes()));
        Self {
            header: Zeroizing::new(format!("Basic {}", encoded.as_str())),
        }
    }

    fn value(&self) -> Option<HeaderValue> {
        let mut value = HeaderValue::from_str(&self.header).ok()?;
        value.set_sensitive(true);
        Some(value)
    }
}

/// What to push: a new branch at `tip` (its objects are the [`PushPack`]
/// [`push_new_branch`] takes).
pub struct PushRequest<'a> {
    /// `https://host/path/repo.git`.
    pub repository_url: &'a Url,
    /// The branch name the model asked for (no `refs/heads/`).
    pub branch: &'a str,
    pub tip: &'a str,
}

/// A git pack with room reserved in front of it for the push command, so
/// the `receive-pack` request body IS the pack's own buffer: the command is
/// written into the headroom and the body is a view of the buffer from
/// there, never a second copy of the pack (which can be as large as one
/// cloud push carries).
pub struct PushPack {
    /// `HEADROOM` reserved bytes, then the pack.
    buffer: Vec<u8>,
}

impl PushPack {
    /// Bytes reserved for the command: its pkt-line (two ids, the ref with
    /// a branch of at most 200 bytes and a `_vN` suffix, the capabilities)
    /// and the flush fit with room to spare.
    pub const HEADROOM: usize = 1_024;

    /// An empty pack buffer, its headroom reserved.
    #[must_use]
    pub fn new() -> Self {
        Self {
            buffer: vec![0; Self::HEADROOM],
        }
    }

    /// A copy of `pack` behind the headroom (fixtures and small packs).
    #[must_use]
    pub fn from_pack(pack: &[u8]) -> Self {
        let mut buffer = Vec::with_capacity(Self::HEADROOM + pack.len());
        buffer.resize(Self::HEADROOM, 0);
        buffer.extend_from_slice(pack);
        Self { buffer }
    }

    /// Append the pack with `write`, which may only append to the buffer it
    /// is handed (the headroom is checked to be intact in length).
    ///
    /// # Errors
    ///
    /// `write`'s own error.
    pub fn append<E>(
        &mut self,
        write: impl FnOnce(&mut Vec<u8>) -> Result<(), E>,
    ) -> Result<(), E> {
        let result = write(&mut self.buffer);
        if self.buffer.len() < Self::HEADROOM {
            self.buffer.resize(Self::HEADROOM, 0);
        }
        result
    }

    /// The pack's bytes.
    #[must_use]
    pub fn as_slice(&self) -> &[u8] {
        &self.buffer[Self::HEADROOM..]
    }

    /// The pack's length in bytes.
    #[must_use]
    pub fn len(&self) -> usize {
        self.buffer.len() - Self::HEADROOM
    }

    /// No pack was written.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// `command` followed by the pack, in the pack's own buffer; `None`
    /// when `command` does not fit the headroom.
    fn into_body(mut self, command: &[u8]) -> Option<bytes::Bytes> {
        let start = Self::HEADROOM.checked_sub(command.len())?;
        self.buffer[start..Self::HEADROOM].copy_from_slice(command);
        Some(bytes::Bytes::from(self.buffer).slice(start..))
    }
}

impl Default for PushPack {
    fn default() -> Self {
        Self::new()
    }
}

/// Why a push did not happen. The sentence names no secret.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PushFailure {
    /// The forge refused the credential (401 or 403).
    Unauthorized,
    /// The repository is not there, or the credential cannot see it.
    NotFound,
    /// Every name up to `_v{MAX_SUFFIX}` is taken.
    NameTaken,
    /// The forge refused the push (its reason, as it said it).
    Rejected(String),
    /// The forge or the network failed; nothing is known to be written.
    Unavailable,
    /// The forge answered something that is not git's protocol.
    Protocol,
}

impl PushFailure {
    #[must_use]
    pub fn sentence(&self) -> String {
        match self {
            Self::Unauthorized => {
                "the forge refused the credential for a push (it needs write access to the repository's contents)".to_owned()
            }
            Self::NotFound => "the forge has no such repository, or the credential cannot see it".to_owned(),
            Self::NameTaken => format!(
                "the branch name and its suffixes up to _v{MAX_SUFFIX} are all taken; name another branch"
            ),
            Self::Rejected(reason) => format!("the forge refused the push: {reason}"),
            Self::Unavailable => "the forge could not be reached; nothing was pushed".to_owned(),
            Self::Protocol => "the forge did not answer as a git server".to_owned(),
        }
    }
}

/// The branch the push created.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pushed {
    /// The name it was created under (`_vN` when the asked one was taken).
    pub branch: String,
}

/// Push `request` over `transport`, only to `request.repository_url`'s
/// origin, as a new branch.
///
/// # Errors
///
/// [`PushFailure`]: nothing was created (a `Rejected` push of a ref that
/// raced into existence included).
pub async fn push_new_branch(
    transport: Arc<dyn Transport>,
    credential: &GitCredential,
    request: &PushRequest<'_>,
    pack: PushPack,
) -> Result<Pushed, PushFailure> {
    if request.repository_url.scheme() != "https" {
        return Err(PushFailure::Protocol);
    }
    let pinned = OriginGuard::new(request.repository_url, transport);
    let refs = advertised_refs(&pinned, credential, request.repository_url).await?;
    let branch = free_name(&refs.names, request.branch)?;
    let mut command = Vec::with_capacity(PushPack::HEADROOM);
    // Only what is read back: no side-band, so the report is plain pkt-lines.
    pkt_line(
        &mut command,
        format!(
            "{ZERO_ID} {} refs/heads/{branch}\0report-status {AGENT}\n",
            request.tip
        )
        .as_bytes(),
    );
    command.extend_from_slice(b"0000");
    // The pack is not copied: the command goes into its headroom.
    let body = pack.into_body(&command).ok_or(PushFailure::Protocol)?;
    let url = service_url(request.repository_url, "git-receive-pack", false);
    let mut http = Request::new(Method::POST, url);
    authorize(&mut http, credential)?;
    http.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/x-git-receive-pack-request"),
    );
    http.headers_mut().insert(
        header::ACCEPT,
        HeaderValue::from_static("application/x-git-receive-pack-result"),
    );
    *http.body_mut() = Some(Body::from(body));
    let mut response = pinned
        .execute(http)
        .await
        .map_err(|_| PushFailure::Unavailable)?;
    status(response.status())?;
    let report = response
        .bytes_within(MAX_REPORT_BYTES)
        .await
        .map_err(|_| PushFailure::Unavailable)?
        .ok_or(PushFailure::Protocol)?;
    read_report(&report, &format!("refs/heads/{branch}"))?;
    Ok(Pushed { branch })
}

struct Advertisement {
    names: BTreeSet<String>,
    capabilities: BTreeSet<String>,
}

fn service_url(repository: &Url, path: &str, advertisement: bool) -> Url {
    let mut url = repository.clone();
    let base = url.path().trim_end_matches('/').to_owned();
    url.set_path(&format!("{base}/{path}"));
    url.set_query(advertisement.then_some("service=git-receive-pack"));
    url.set_fragment(None);
    url
}

fn authorize(request: &mut Request, credential: &GitCredential) -> Result<(), PushFailure> {
    let value = credential.value().ok_or(PushFailure::Unauthorized)?;
    request.headers_mut().insert(header::AUTHORIZATION, value);
    Ok(())
}

fn status(status: StatusCode) -> Result<(), PushFailure> {
    match status.as_u16() {
        200 => Ok(()),
        401 | 403 => Err(PushFailure::Unauthorized),
        404 => Err(PushFailure::NotFound),
        500.. => Err(PushFailure::Unavailable),
        _ => Err(PushFailure::Protocol),
    }
}

async fn advertised_refs(
    transport: &OriginGuard,
    credential: &GitCredential,
    repository: &Url,
) -> Result<Advertisement, PushFailure> {
    let mut http = Request::new(Method::GET, service_url(repository, "info/refs", true));
    authorize(&mut http, credential)?;
    http.headers_mut()
        .insert(header::ACCEPT, HeaderValue::from_static("*/*"));
    let mut response = transport
        .execute(http)
        .await
        .map_err(|_| PushFailure::Unavailable)?;
    status(response.status())?;
    let content_type = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_ascii_lowercase();
    // A dumb server (or a login page) is not a receive-pack endpoint.
    if !content_type.starts_with("application/x-git-receive-pack-advertisement") {
        return Err(PushFailure::Protocol);
    }
    let body = response
        .bytes_within(MAX_ADVERTISEMENT_BYTES)
        .await
        .map_err(|_| PushFailure::Unavailable)?
        .ok_or(PushFailure::Protocol)?;
    parse_advertisement(&body)
}

/// Append one pkt-line.
fn pkt_line(out: &mut Vec<u8>, data: &[u8]) {
    out.extend_from_slice(format!("{:04x}", data.len() + 4).as_bytes());
    out.extend_from_slice(data);
}

/// The pkt-lines of `bytes`: `None` for a flush.
fn pkt_lines(bytes: &[u8]) -> Result<Vec<Option<&[u8]>>, PushFailure> {
    let mut lines = Vec::new();
    let mut rest = bytes;
    while !rest.is_empty() {
        let length = rest
            .get(..4)
            .and_then(|head| std::str::from_utf8(head).ok())
            .and_then(|head| usize::from_str_radix(head, 16).ok())
            .ok_or(PushFailure::Protocol)?;
        if length == 0 {
            lines.push(None);
            rest = &rest[4..];
            continue;
        }
        if length < 4 {
            return Err(PushFailure::Protocol);
        }
        let line = rest.get(4..length).ok_or(PushFailure::Protocol)?;
        lines.push(Some(line));
        rest = &rest[length..];
    }
    Ok(lines)
}

fn parse_advertisement(bytes: &[u8]) -> Result<Advertisement, PushFailure> {
    let lines = pkt_lines(bytes)?;
    let mut lines = lines.into_iter();
    // `# service=git-receive-pack`, then a flush.
    match lines.next() {
        Some(Some(line)) if line.trim_ascii_end() == b"# service=git-receive-pack" => {}
        _ => return Err(PushFailure::Protocol),
    }
    if lines.next() != Some(None) {
        return Err(PushFailure::Protocol);
    }
    let mut advertisement = Advertisement {
        names: BTreeSet::new(),
        capabilities: BTreeSet::new(),
    };
    for (index, line) in lines.enumerate() {
        let Some(line) = line else { break };
        let line = line.trim_ascii_end();
        let (reference, capabilities) = match line.iter().position(|byte| *byte == 0) {
            Some(at) => (&line[..at], Some(&line[at + 1..])),
            None => (line, None),
        };
        if let (0, Some(capabilities)) = (index, capabilities) {
            for capability in String::from_utf8_lossy(capabilities).split_ascii_whitespace() {
                let name = capability.split('=').next().unwrap_or(capability);
                advertisement.capabilities.insert(name.to_owned());
            }
        }
        let text = std::str::from_utf8(reference).map_err(|_| PushFailure::Protocol)?;
        let (_, name) = text.split_once(' ').ok_or(PushFailure::Protocol)?;
        // An empty repository advertises `capabilities^{}` with a zero id.
        if name != "capabilities^{}" {
            advertisement.names.insert(name.to_owned());
        }
    }
    if !advertisement.capabilities.contains("report-status") {
        return Err(PushFailure::Protocol);
    }
    Ok(advertisement)
}

/// `branch`, or the first `branch_vN` the remote does not have. A name is
/// taken when it is a ref, or when a ref lives under it (`a` when `a/b`
/// exists) or it lives under a ref (`a/b` when `a` exists): git refuses both.
fn free_name(names: &BTreeSet<String>, branch: &str) -> Result<String, PushFailure> {
    let taken = |candidate: &str| {
        let full = format!("refs/heads/{candidate}");
        names.iter().any(|name| {
            name.eq_ignore_ascii_case(&full)
                || name.starts_with(&format!("{full}/"))
                || full.starts_with(&format!("{name}/"))
        })
    };
    if !taken(branch) {
        return Ok(branch.to_owned());
    }
    (2..=MAX_SUFFIX)
        .map(|suffix| format!("{branch}_v{suffix}"))
        .find(|candidate| !taken(candidate))
        .ok_or(PushFailure::NameTaken)
}

/// Read `report-status`: `unpack ok`, then `ok <ref>` for the one command.
fn read_report(bytes: &[u8], reference: &str) -> Result<(), PushFailure> {
    let mut unpacked = false;
    for line in pkt_lines(bytes)?.into_iter().flatten() {
        let text = String::from_utf8_lossy(line);
        let text = text.trim_end();
        if let Some(status) = text.strip_prefix("unpack ") {
            if status != "ok" {
                return Err(PushFailure::Rejected(bounded(&format!(
                    "the pack was not accepted ({status}); the upstream commit may be missing from the remote, so push it from the desktop first"
                ))));
            }
            unpacked = true;
        } else if let Some(name) = text.strip_prefix("ok ") {
            if unpacked && name == reference {
                return Ok(());
            }
        } else if let Some(rest) = text.strip_prefix("ng ") {
            let reason = rest.strip_prefix(reference).map_or(rest, str::trim_start);
            return Err(PushFailure::Rejected(bounded(reason)));
        }
    }
    Err(PushFailure::Protocol)
}

fn bounded(text: &str) -> String {
    text.chars()
        .filter(|character| !character.is_control())
        .take(300)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn advertisement(refs: &[&str], capabilities: &str) -> Vec<u8> {
        let mut out = Vec::new();
        pkt_line(&mut out, b"# service=git-receive-pack\n");
        out.extend_from_slice(b"0000");
        for (index, name) in refs.iter().enumerate() {
            let line = if index == 0 {
                format!("{} {name}\0{capabilities}\n", "1".repeat(40))
            } else {
                format!("{} {name}\n", "1".repeat(40))
            };
            pkt_line(&mut out, line.as_bytes());
        }
        out.extend_from_slice(b"0000");
        out
    }

    #[test]
    fn an_advertisement_names_its_refs_and_capabilities() {
        let parsed = parse_advertisement(&advertisement(
            &["refs/heads/main", "refs/heads/feature/x"],
            "report-status delete-refs side-band-64k agent=git/2",
        ))
        .expect("parsed");
        assert!(parsed.names.contains("refs/heads/main"));
        assert!(parsed.capabilities.contains("report-status"));
        assert!(parsed.capabilities.contains("agent"));
        let empty = advertisement(&["capabilities^{}"], "report-status");
        assert!(
            parse_advertisement(&empty)
                .expect("empty repo")
                .names
                .is_empty()
        );
        // No report-status: the outcome could not be read.
        assert_eq!(
            parse_advertisement(&advertisement(&["refs/heads/main"], "delete-refs")).err(),
            Some(PushFailure::Protocol)
        );
        assert_eq!(
            parse_advertisement(b"<html>").err(),
            Some(PushFailure::Protocol)
        );
    }

    #[test]
    fn a_taken_name_gets_the_next_free_suffix() {
        let names: BTreeSet<String> = [
            "refs/heads/main",
            "refs/heads/fix",
            "refs/heads/fix_v2",
            "refs/heads/area/one",
        ]
        .into_iter()
        .map(str::to_owned)
        .collect();
        assert_eq!(free_name(&names, "new").as_deref(), Ok("new"));
        assert_eq!(free_name(&names, "fix").as_deref(), Ok("fix_v3"));
        assert_eq!(free_name(&names, "MAIN").as_deref(), Ok("MAIN_v2"));
        // A directory of refs, and a ref's own subdirectory, are taken too.
        assert_eq!(free_name(&names, "area").as_deref(), Ok("area_v2"));
        // Under an existing ref no suffix helps: name another branch.
        assert_eq!(free_name(&names, "main/sub"), Err(PushFailure::NameTaken));
    }

    #[test]
    fn a_report_is_read_for_the_one_ref() {
        let reference = "refs/heads/x";
        let mut ok = Vec::new();
        pkt_line(&mut ok, b"unpack ok\n");
        pkt_line(&mut ok, b"ok refs/heads/x\n");
        ok.extend_from_slice(b"0000");
        assert_eq!(read_report(&ok, reference), Ok(()));
        let mut ng = Vec::new();
        pkt_line(&mut ng, b"unpack ok\n");
        pkt_line(&mut ng, b"ng refs/heads/x protected branch hook declined\n");
        assert_eq!(
            read_report(&ng, reference),
            Err(PushFailure::Rejected(
                "protected branch hook declined".to_owned()
            ))
        );
        let mut missing = Vec::new();
        pkt_line(&mut missing, b"unpack index-pack abnormal exit\n");
        assert!(matches!(
            read_report(&missing, reference),
            Err(PushFailure::Rejected(reason)) if reason.contains("upstream commit")
        ));
        assert_eq!(read_report(b"0000", reference), Err(PushFailure::Protocol));
    }

    #[test]
    fn the_credential_is_basic_and_never_printed() {
        let credential = GitCredential::basic("x-access-token", "ghp_secret");
        assert_eq!(format!("{credential:?}"), "GitCredential(..)");
        let value = credential.value().expect("a header");
        assert!(value.is_sensitive());
        assert_eq!(
            value.to_str().expect("ascii"),
            format!(
                "Basic {}",
                base64::engine::general_purpose::STANDARD.encode("x-access-token:ghp_secret")
            )
        );
    }

    /// The body is the command, then the pack, from the pack's own buffer.
    #[test]
    fn the_body_is_the_command_in_the_packs_headroom() {
        let mut pack = PushPack::new();
        pack.append(|buffer| {
            buffer.extend_from_slice(b"PACK-bytes");
            Ok::<(), ()>(())
        })
        .expect("appended");
        assert_eq!(pack.as_slice(), b"PACK-bytes");
        assert_eq!(pack.len(), 10);
        let address = pack.as_slice().as_ptr();
        let body = pack.into_body(b"0008cmd0000").expect("fits");
        assert_eq!(&body[..], b"0008cmd0000PACK-bytes");
        // The same allocation: the pack was not copied.
        assert_eq!(body[11..].as_ptr(), address);
        let longest = vec![b'x'; PushPack::HEADROOM + 1];
        assert!(PushPack::from_pack(b"PACK").into_body(&longest).is_none());
        // The longest command a push builds fits.
        let branch = "b".repeat(200) + "_v20";
        let mut command = Vec::new();
        pkt_line(
            &mut command,
            format!("{ZERO_ID} {ZERO_ID} refs/heads/{branch}\0report-status {AGENT}\n").as_bytes(),
        );
        command.extend_from_slice(b"0000");
        assert!(command.len() <= PushPack::HEADROOM);
    }

    #[test]
    fn service_urls_hang_off_the_repository() {
        let repository = Url::parse("https://git.example.com/group/sub/repo.git").expect("url");
        assert_eq!(
            service_url(&repository, "info/refs", true).as_str(),
            "https://git.example.com/group/sub/repo.git/info/refs?service=git-receive-pack"
        );
        assert_eq!(
            service_url(&repository, "git-receive-pack", false).as_str(),
            "https://git.example.com/group/sub/repo.git/git-receive-pack"
        );
    }
}
