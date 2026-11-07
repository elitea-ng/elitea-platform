//! The engine process: chrome-headless-shell (`full`) or Moli (`lite`) with
//! the flags ADR-0049 decisions 1, 2, 4 and 5 require, and CDP to it.
//!
//! **Moli** (`lite`, admitted only by the bake-off of decision 9) has no
//! debugging pipe: `moli serve` listens for CDP on a WebSocket. It is bound
//! to the session container's loopback on a port chosen here, CDP is
//! carried over [`crate::cdp::websocket_bridge`], and the page's own traffic
//! still goes only to the egress proxy (`--http-proxy`, no bypass list), so
//! the page cannot reach that port. Moli has no process sandbox of its own:
//! [`crate::session`] starts it only under gVisor. It has no WebRTC, QUIC or
//! DNS-over-HTTPS client; the proxy resolves every name. The rest of this
//! list is chrome-headless-shell's:
//!
//! * **CDP never leaves the process pair.** `--remote-debugging-pipe` and
//!   never `--remote-debugging-port`: there is no listener, not even on the
//!   pod's loopback. [`EngineConfig::args`] refuses any extra argument that
//!   would open one.
//! * **Egress only through the proxy.** `--proxy-server` names the egress
//!   proxy, and `--proxy-bypass-list=<-loopback>` removes Chromium's implicit
//!   loopback bypass, so even `localhost` goes to the proxy (which refuses
//!   it). The proxy's run token is answered from `Fetch.authRequired`, never
//!   put on the command line.
//! * **No side channel past the proxy.** QUIC off (`--disable-quic`),
//!   WebRTC limited to proxied TCP
//!   (`--force-webrtc-ip-handling-policy=disable_non_proxied_udp`), DNS over
//!   HTTPS off (`DnsOverHttps` disabled; with a proxy the proxy resolves
//!   names anyway). The sandbox network policy is the outer fence: the pod
//!   can reach nothing but the proxy.
//! * **The engine's own sandbox stays on**: `--no-sandbox` only when the
//!   container runs under gVisor, where the container is the boundary
//!   (decision 1). The supervisor's profile refuses the flag on a shared
//!   kernel; this binary refuses it unless told the runtime is gVisor.

use std::os::fd::OwnedFd;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};

use command_fds::{CommandFdExt, FdMapping};
use tokio::net::unix::pipe;
use tokio::process::{Child, Command};

use crate::cdp::Connection;

/// The image's engine.
pub const DEFAULT_ENGINE: &str = "/opt/chrome-headless-shell/chrome-headless-shell";
/// The image's Moli (`lite`).
pub const DEFAULT_MOLI: &str = "/opt/moli/moli";
/// How long Moli may take to answer its discovery endpoint.
const MOLI_READY: std::time::Duration = std::time::Duration::from_secs(20);

/// Which engine a binary is.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum EngineKind {
    /// chrome-headless-shell: CDP on `--remote-debugging-pipe`.
    #[default]
    Chrome,
    /// Moli: CDP on a loopback WebSocket.
    Moli,
}

impl EngineKind {
    /// The product name in `engine_version` (`<name>/<version>`).
    #[must_use]
    pub const fn product(self) -> &'static str {
        match self {
            Self::Chrome => "chrome-headless-shell",
            Self::Moli => "moli",
        }
    }
}
/// The view's default size (ADR-0049 decision 6).
pub const DEFAULT_WIDTH: u32 = 1280;
pub const DEFAULT_HEIGHT: u32 = 800;

/// Where an offline engine's traffic goes: the discard port on loopback,
/// where nothing listens in the session container.
const OFFLINE_PROXY: &str = "http://127.0.0.1:9";

/// Features switched off: DNS over HTTPS, the back-forward cache (a history
/// step is always a real load, so refs always go stale with it), and the
/// background services that phone home.
const DISABLED_FEATURES: &str = "DnsOverHttps,BackForwardCache,Translate,MediaRouter,OptimizationHints,AutofillServerCommunication,CalculateNativeWinOcclusion,InterestFeedContentSuggestions,PrivacySandboxSettings4";

/// How to start the engine.
#[derive(Clone, Debug)]
pub struct EngineConfig {
    pub engine: PathBuf,
    /// What `engine` is.
    pub kind: EngineKind,
    /// The egress proxy, `http://host:port`; `None` only in tests.
    pub proxy: Option<String>,
    /// The run token the proxy wants (`Proxy-Authorization: Basic run:<token>`).
    pub proxy_token: Option<String>,
    /// The container runs under gVisor: the engine may run without its own
    /// sandbox.
    pub gvisor: bool,
    pub width: u32,
    pub height: u32,
    /// The engine's stderr goes to ours (else it is discarded).
    pub engine_log: bool,
    /// No network at all (the `start` line's `proxy: null`): every request,
    /// loopback included, goes to a proxy address nothing listens on.
    pub offline: bool,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            engine: PathBuf::from(DEFAULT_ENGINE),
            kind: EngineKind::Chrome,
            proxy: None,
            proxy_token: None,
            gvisor: false,
            width: DEFAULT_WIDTH,
            height: DEFAULT_HEIGHT,
            engine_log: false,
            offline: false,
        }
    }
}

/// A configuration the agent will not start.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigError(pub String);

impl std::fmt::Display for ConfigError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl std::error::Error for ConfigError {}

impl EngineConfig {
    /// The engine's arguments, `user_data_dir` being its throwaway profile.
    ///
    /// # Errors
    ///
    /// A proxy that is not `http://host:port` (credentials in it included:
    /// the token never goes on a command line), or a view size outside 320
    /// to 3840 by 240 to 2160.
    pub fn args(&self, user_data_dir: &Path) -> Result<Vec<String>, ConfigError> {
        if !(320..=3840).contains(&self.width) || !(240..=2160).contains(&self.height) {
            return Err(ConfigError("the view size is out of range".to_owned()));
        }
        if self.kind == EngineKind::Moli {
            return Err(ConfigError("Moli takes moli_args".to_owned()));
        }
        let mut args = vec![
            "--remote-debugging-pipe".to_owned(),
            format!("--user-data-dir={}", user_data_dir.display()),
            format!("--window-size={},{}", self.width, self.height),
            "--no-first-run".to_owned(),
            "--no-default-browser-check".to_owned(),
            "--disable-background-networking".to_owned(),
            "--disable-component-update".to_owned(),
            "--disable-default-apps".to_owned(),
            "--disable-extensions".to_owned(),
            "--disable-sync".to_owned(),
            "--disable-breakpad".to_owned(),
            "--disable-domain-reliability".to_owned(),
            "--disable-client-side-phishing-detection".to_owned(),
            "--metrics-recording-only".to_owned(),
            "--no-pings".to_owned(),
            "--mute-audio".to_owned(),
            "--hide-scrollbars".to_owned(),
            "--password-store=basic".to_owned(),
            "--use-mock-keychain".to_owned(),
            // The container's /dev/shm is small.
            "--disable-dev-shm-usage".to_owned(),
            "--disable-quic".to_owned(),
            "--force-webrtc-ip-handling-policy=disable_non_proxied_udp".to_owned(),
            "--webrtc-ip-handling-policy=disable_non_proxied_udp".to_owned(),
            format!("--disable-features={DISABLED_FEATURES}"),
        ];
        match (&self.proxy, self.offline) {
            (Some(proxy), _) => {
                check_proxy(proxy)?;
                args.push(format!("--proxy-server={proxy}"));
                args.push("--proxy-bypass-list=<-loopback>".to_owned());
            }
            (None, true) => {
                args.push(format!("--proxy-server={OFFLINE_PROXY}"));
                args.push("--proxy-bypass-list=<-loopback>".to_owned());
            }
            // Tests only: the conformance kit's fixtures on loopback.
            (None, false) => {}
        }
        if self.gvisor {
            args.push("--no-sandbox".to_owned());
        }
        args.push("about:blank".to_owned());
        Ok(args)
    }
}

impl EngineConfig {
    /// Moli's `serve` arguments, CDP on `127.0.0.1:<port>`.
    ///
    /// * `--layout`: real geometry, coordinate input, screenshots and the
    ///   screencast, which the contract needs.
    /// * `--http-proxy` names the egress proxy and `--http-no-proxy` is left
    ///   unset; with a cleared environment there is no bypass at all, so
    ///   loopback, the CDP port included, also goes to the proxy, which
    ///   refuses it. The run token is answered from `Fetch.authRequired`,
    ///   as for Chrome, never put on the command line.
    /// * No `--profile-dir`, `--http-cache-dir` or cookie file: storage is
    ///   temporary and dies with the session.
    ///
    /// # Errors
    ///
    /// As [`Self::args`].
    pub fn moli_args(&self, port: u16) -> Result<Vec<String>, ConfigError> {
        if !(320..=3840).contains(&self.width) || !(240..=2160).contains(&self.height) {
            return Err(ConfigError("the view size is out of range".to_owned()));
        }
        let mut args = vec![
            "serve".to_owned(),
            "--host".to_owned(),
            "127.0.0.1".to_owned(),
            "--port".to_owned(),
            port.to_string(),
            "--layout".to_owned(),
            "--screencast-interval-ms".to_owned(),
            "100".to_owned(),
        ];
        match (&self.proxy, self.offline) {
            (Some(proxy), _) => {
                check_proxy(proxy)?;
                args.push("--http-proxy".to_owned());
                args.push(proxy.clone());
            }
            (None, true) => {
                args.push("--http-proxy".to_owned());
                args.push(OFFLINE_PROXY.to_owned());
            }
            // Tests only: the conformance kit's fixtures on loopback.
            (None, false) => {}
        }
        Ok(args)
    }
}

fn check_proxy(proxy: &str) -> Result<(), ConfigError> {
    let refused = || ConfigError("the proxy must be http://host:port".to_owned());
    let rest = proxy.strip_prefix("http://").ok_or_else(refused)?;
    let authority = rest.strip_suffix('/').unwrap_or(rest);
    let valid = !authority.is_empty()
        && !authority.contains(['@', '/', '?', '#'])
        && authority.rsplit_once(':').is_some_and(|(host, port)| {
            !host.is_empty() && port.parse::<u16>().is_ok_and(|port| port > 0)
        })
        && !authority
            .chars()
            .any(|ch| ch.is_whitespace() || ch.is_control());
    if valid { Ok(()) } else { Err(refused()) }
}

/// A running engine. Dropping it kills the engine and removes its profile.
pub struct Engine {
    child: Child,
    user_data_dir: PathBuf,
}

impl Engine {
    /// Start the engine and connect to its debugging pipe.
    ///
    /// # Errors
    ///
    /// A refused configuration, or the engine failing to start.
    pub async fn start(config: &EngineConfig) -> Result<(Self, Connection), std::io::Error> {
        static SEQUENCE: AtomicU64 = AtomicU64::new(0);
        let user_data_dir = std::env::temp_dir().join(format!(
            "elitea-browser-{}-{}",
            std::process::id(),
            SEQUENCE.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&user_data_dir)?;
        if config.kind == EngineKind::Moli {
            return Self::start_moli(config, user_data_dir).await;
        }
        let args = config
            .args(&user_data_dir)
            .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidInput, error))?;
        // fd 3: the engine reads commands; fd 4: it writes replies and events.
        let (to_engine, engine_reads) = pipe::pipe()?;
        let (engine_writes, from_engine) = pipe::pipe()?;
        let engine_reads: OwnedFd = engine_reads.into_blocking_fd()?;
        let engine_writes: OwnedFd = engine_writes.into_blocking_fd()?;
        let mut command = Command::new(&config.engine);
        command
            .args(&args)
            .env_clear()
            .env("HOME", &user_data_dir)
            .stdin(Stdio::null())
            // Our stdout is the supervisor protocol: the engine never writes
            // to it.
            .stdout(Stdio::null())
            .stderr(if config.engine_log {
                Stdio::inherit()
            } else {
                Stdio::null()
            })
            .kill_on_drop(true);
        command
            .fd_mappings(vec![
                FdMapping {
                    parent_fd: engine_reads,
                    child_fd: 3,
                },
                FdMapping {
                    parent_fd: engine_writes,
                    child_fd: 4,
                },
            ])
            .map_err(|_| std::io::Error::other("the engine's pipe descriptors collide"))?;
        let child = command.spawn()?;
        // The engine's ends now live in the child; ours close with `command`.
        drop(command);
        let connection = Connection::new(from_engine, to_engine);
        Ok((
            Self {
                child,
                user_data_dir,
            },
            connection,
        ))
    }

    async fn start_moli(
        config: &EngineConfig,
        user_data_dir: PathBuf,
    ) -> Result<(Self, Connection), std::io::Error> {
        // A free loopback port: bound, read and released. The container is
        // the session's alone, so nothing else races for it.
        let port = std::net::TcpListener::bind("127.0.0.1:0")?
            .local_addr()?
            .port();
        let args = config
            .moli_args(port)
            .map_err(|error| std::io::Error::new(std::io::ErrorKind::InvalidInput, error))?;
        let child = Command::new(&config.engine)
            .args(&args)
            .env_clear()
            .env("HOME", &user_data_dir)
            .env("TMPDIR", &user_data_dir)
            .current_dir(&user_data_dir)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(if config.engine_log {
                Stdio::inherit()
            } else {
                Stdio::null()
            })
            .kill_on_drop(true)
            .spawn()?;
        // From here a failure drops `engine`, which kills Moli.
        let engine = Self {
            child,
            user_data_dir,
        };
        let url = tokio::time::timeout(MOLI_READY, moli_endpoint(port))
            .await
            .map_err(|_| std::io::Error::other("Moli did not answer in time"))?;
        let stream = tokio::net::TcpStream::connect(("127.0.0.1", port)).await?;
        let (socket, _) = tokio_tungstenite::client_async(url.as_str(), stream)
            .await
            .map_err(|_| std::io::Error::other("Moli refused the CDP connection"))?;
        let (reads, writes) = crate::cdp::websocket_bridge(socket);
        Ok((engine, Connection::new(reads, writes)))
    }

    /// Whether the engine is still running.
    pub fn is_running(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }

    /// The engine's process id.
    #[must_use]
    pub fn pid(&self) -> Option<u32> {
        self.child.id()
    }
}

/// Poll Moli's discovery endpoint until it answers with its browser
/// WebSocket URL on `127.0.0.1:<port>`.
async fn moli_endpoint(port: u16) -> String {
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
    loop {
        if let Ok(mut stream) = tokio::net::TcpStream::connect(("127.0.0.1", port)).await {
            let request = format!(
                "GET /json/version HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n"
            );
            if stream.write_all(request.as_bytes()).await.is_ok() {
                let mut body = Vec::new();
                let mut limited = (&mut stream).take(64 << 10);
                if limited.read_to_end(&mut body).await.is_ok()
                    && let Some(url) = websocket_url(&body, port)
                {
                    return url;
                }
            }
        }
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    }
}

/// `webSocketDebuggerUrl` from a `/json/version` response, only when it
/// names the engine's own loopback port.
fn websocket_url(response: &[u8], port: u16) -> Option<String> {
    let text = std::str::from_utf8(response).ok()?;
    let (head, body) = text.split_once("\r\n\r\n")?;
    if !head.starts_with("HTTP/1.1 200") && !head.starts_with("HTTP/1.0 200") {
        return None;
    }
    let version: serde_json::Value = serde_json::from_str(body.trim()).ok()?;
    let url = version["webSocketDebuggerUrl"].as_str()?;
    let prefix = format!("ws://127.0.0.1:{port}/");
    url.starts_with(&prefix).then(|| url.to_owned())
}

impl Drop for Engine {
    fn drop(&mut self) {
        let _ = self.child.start_kill();
        let _ = std::fs::remove_dir_all(&self.user_data_dir);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(config: &EngineConfig) -> Vec<String> {
        config.args(Path::new("/tmp/profile")).unwrap()
    }

    #[test]
    fn cdp_is_a_pipe_and_never_a_port() {
        let args = args(&EngineConfig::default());
        assert!(args.contains(&"--remote-debugging-pipe".to_owned()));
        assert!(
            !args
                .iter()
                .any(|arg| arg.starts_with("--remote-debugging-port"))
        );
        assert!(
            !args
                .iter()
                .any(|arg| arg.starts_with("--remote-debugging-address"))
        );
    }

    #[test]
    fn the_sandbox_stays_on_unless_gvisor() {
        assert!(!args(&EngineConfig::default()).contains(&"--no-sandbox".to_owned()));
        let gvisor = EngineConfig {
            gvisor: true,
            ..EngineConfig::default()
        };
        assert!(args(&gvisor).contains(&"--no-sandbox".to_owned()));
    }

    #[test]
    fn side_channels_are_off_and_the_proxy_takes_loopback_too() {
        let config = EngineConfig {
            proxy: Some("http://elitea-egress-proxy.elitea.svc:3128".to_owned()),
            ..EngineConfig::default()
        };
        let args = args(&config);
        for flag in [
            "--disable-quic",
            "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
            "--proxy-server=http://elitea-egress-proxy.elitea.svc:3128",
            "--proxy-bypass-list=<-loopback>",
            "--window-size=1280,800",
        ] {
            assert!(args.contains(&flag.to_owned()), "{flag}");
        }
        let features = args
            .iter()
            .find_map(|arg| arg.strip_prefix("--disable-features="))
            .unwrap();
        assert!(features.split(',').any(|feature| feature == "DnsOverHttps"));
        // The token is never on the command line.
        let with_token = EngineConfig {
            proxy_token: Some("secret-token".to_owned()),
            ..config
        };
        assert!(
            !super::tests::args(&with_token)
                .iter()
                .any(|arg| arg.contains("secret-token"))
        );
    }

    #[test]
    fn offline_sends_everything_nowhere() {
        let config = EngineConfig {
            offline: true,
            ..EngineConfig::default()
        };
        let args = args(&config);
        assert!(args.contains(&"--proxy-server=http://127.0.0.1:9".to_owned()));
        assert!(args.contains(&"--proxy-bypass-list=<-loopback>".to_owned()));
    }

    #[test]
    fn moli_listens_on_loopback_and_sends_everything_to_the_proxy() {
        let config = EngineConfig {
            kind: EngineKind::Moli,
            proxy: Some("http://10.0.0.7:3128".to_owned()),
            proxy_token: Some("secret-token".to_owned()),
            ..EngineConfig::default()
        };
        let joined = config.moli_args(40123).unwrap().join(" ");
        assert!(joined.starts_with("serve --host 127.0.0.1 --port 40123 --layout"));
        assert!(joined.contains("--http-proxy http://10.0.0.7:3128"));
        assert!(!joined.contains("--http-no-proxy"));
        assert!(!joined.contains("secret-token"));
        assert!(!joined.contains("--profile-dir") && !joined.contains("--cookie"));
        let offline = EngineConfig {
            kind: EngineKind::Moli,
            offline: true,
            ..EngineConfig::default()
        };
        let offline = offline.moli_args(1).unwrap().join(" ");
        assert!(offline.contains("--http-proxy http://127.0.0.1:9"));
        // Moli takes no Chrome flags.
        assert!(config.args(Path::new("/tmp/p")).is_err());
        let bad = EngineConfig {
            kind: EngineKind::Moli,
            proxy: Some("http://run:t@proxy:3128".to_owned()),
            ..EngineConfig::default()
        };
        assert!(bad.moli_args(1).is_err());
    }

    #[test]
    fn the_moli_endpoint_must_be_its_own_loopback_port() {
        let ok = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"Browser\":\"Moli/1.1.22\",\"webSocketDebuggerUrl\":\"ws://127.0.0.1:9333/devtools/browser/abc\"}";
        assert_eq!(
            websocket_url(ok, 9333).as_deref(),
            Some("ws://127.0.0.1:9333/devtools/browser/abc")
        );
        assert_eq!(websocket_url(ok, 9334), None);
        let elsewhere =
            b"HTTP/1.1 200 OK\r\n\r\n{\"webSocketDebuggerUrl\":\"ws://10.0.0.1:9333/x\"}";
        assert_eq!(websocket_url(elsewhere, 9333), None);
        assert_eq!(
            websocket_url(b"HTTP/1.1 404 Not Found\r\n\r\n{}", 9333),
            None
        );
    }

    #[test]
    fn a_proxy_with_credentials_or_a_path_is_refused() {
        for proxy in [
            "http://run:token@proxy:3128",
            "https://proxy:3128",
            "socks5://proxy:1080",
            "http://proxy",
            "http://proxy:0",
            "http://proxy:3128/path",
            "http://:3128",
        ] {
            let config = EngineConfig {
                proxy: Some(proxy.to_owned()),
                ..EngineConfig::default()
            };
            assert!(config.args(Path::new("/tmp/p")).is_err(), "{proxy}");
        }
        let fine = EngineConfig {
            proxy: Some("http://10.0.0.1:3128/".to_owned()),
            ..EngineConfig::default()
        };
        assert!(fine.args(Path::new("/tmp/p")).is_ok());
    }
}
