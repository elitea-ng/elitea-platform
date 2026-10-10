//! Certificate rotation without a restart.
//!
//! Certificates here are short-lived and re-issued by the deployment
//! (cert-manager, a sidecar, a mounted secret). Three things are reloaded
//! from their files every [`RELOAD_INTERVAL`]:
//!
//! * the **serving** certificate and key of the gRPC listener
//!   ([`ReloadingCertificate`], a rustls certificate resolver: a new
//!   handshake presents the new certificate, connections already open keep
//!   the old one until they end);
//! * the **client** certificate and key to elitea-main's introspection
//!   service (a tonic channel pins its identity at build time, so
//!   [`spawn_channel_reloader`] rebuilds the channel when the files, or the
//!   CA, change and swaps it into the [`GrpcIntrospector`]);
//! * the **client CA bundle** of the listener ([`ReloadingClientCa`], a
//!   rustls client-certificate verifier that delegates to a verifier
//!   rebuilt from the bundle and swapped atomically: a new handshake is
//!   checked against the new bundle, open connections are not re-checked).
//!
//! A reload that fails (a half-written file, a key that does not match the
//! certificate) is logged and the previous material keeps serving; the next
//! tick tries again. `/readyz` reports not-ready when the serving certificate
//! expires within [`MIN_VALIDITY_SECONDS`] ([`ReloadingCertificate::
//! seconds_until_expiry`]), so a rotation that stopped is noticed before the
//! certificate dies.
//!
//! A CA bundle that cannot be used (unreadable, no certificate, not PEM) is
//! logged and the previous bundle keeps verifying. The CA of elitea-main (the
//! trust anchor of the introspection channel) follows its file through the
//! channel reloader. Rotating a CA is still an overlap: publish a bundle
//! holding both the old and the new CA, re-issue the certificates, then drop
//! the old CA.

use std::fmt;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwap;
use rustls::RootCertStore;
use rustls::ServerConfig;
use rustls::pki_types::pem::PemObject as _;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::server::danger::{ClientCertVerified, ClientCertVerifier};
use rustls::server::{ClientHello, ResolvesServerCert, WebPkiClientVerifier};
use rustls::sign::CertifiedKey;
use sha2::{Digest as _, Sha256};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{Semaphore, mpsc};
use tokio_rustls::TlsAcceptor;
use tokio_rustls::server::TlsStream;
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use x509_parser::prelude::{FromDer as _, X509Certificate};

use crate::auth::GrpcIntrospector;

/// How often the certificate files are looked at.
pub const RELOAD_INTERVAL: Duration = Duration::from_mins(1);
/// `/readyz` fails when the serving certificate expires within this long.
pub const MIN_VALIDITY_SECONDS: i64 = 3600;
/// The longest a TLS handshake may take.
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
/// The most handshakes in flight at once; a connection beyond it is dropped.
const MAX_HANDSHAKES: usize = 256;

/// Certificate material that could not be loaded.
#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct TlsError(String);

fn read(path: &Path) -> Result<Vec<u8>, TlsError> {
    std::fs::read(path).map_err(|error| TlsError(format!("read {}: {error}", path.display())))
}

fn digest(parts: &[&[u8]]) -> [u8; 32] {
    let mut hasher = Sha256::new();
    for part in parts {
        // Length-prefixed so moving a byte between parts changes the digest.
        hasher.update((part.len() as u64).to_be_bytes());
        hasher.update(part);
    }
    hasher.finalize().into()
}

struct Loaded {
    key: Arc<CertifiedKey>,
    digest: [u8; 32],
    not_after_unix: i64,
}

fn parse(cert_pem: &[u8], key_pem: &[u8]) -> Result<Loaded, TlsError> {
    let chain = CertificateDer::pem_slice_iter(cert_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| TlsError(format!("certificate PEM: {error}")))?;
    let leaf = chain
        .first()
        .ok_or_else(|| TlsError("the certificate file holds no certificate".to_owned()))?;
    let (_, parsed) = X509Certificate::from_der(leaf.as_ref())
        .map_err(|error| TlsError(format!("certificate: {error}")))?;
    let not_after_unix = parsed.validity().not_after.timestamp();
    let private = PrivateKeyDer::from_pem_slice(key_pem)
        .map_err(|error| TlsError(format!("private key PEM: {error}")))?;
    let signing = rustls::crypto::ring::sign::any_supported_type(&private)
        .map_err(|error| TlsError(format!("private key: {error}")))?;
    let key = CertifiedKey::new(chain, signing);
    key.keys_match()
        .map_err(|error| TlsError(format!("the key does not match the certificate: {error}")))?;
    Ok(Loaded {
        key: Arc::new(key),
        digest: digest(&[cert_pem, key_pem]),
        not_after_unix,
    })
}

/// A serving certificate that follows its files.
pub struct ReloadingCertificate {
    cert_file: PathBuf,
    key_file: PathBuf,
    current: ArcSwap<Loaded>,
}

impl fmt::Debug for ReloadingCertificate {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReloadingCertificate")
            .field("cert_file", &self.cert_file)
            .field("key_file", &self.key_file)
            .finish_non_exhaustive()
    }
}

impl ReloadingCertificate {
    /// Loads the pair; the process does not start without a usable one.
    ///
    /// # Errors
    /// [`TlsError`] for an unreadable or invalid pair.
    pub fn load(cert_file: &Path, key_file: &Path) -> Result<Arc<Self>, TlsError> {
        let loaded = parse(&read(cert_file)?, &read(key_file)?)?;
        Ok(Arc::new(Self {
            cert_file: cert_file.to_owned(),
            key_file: key_file.to_owned(),
            current: ArcSwap::from_pointee(loaded),
        }))
    }

    /// Re-reads the files and, when they changed, swaps the new certificate
    /// in atomically. `Ok(true)` when it swapped.
    ///
    /// # Errors
    /// [`TlsError`] when the files cannot be read or are not a valid pair;
    /// the previous certificate stays in service.
    pub fn reload(&self) -> Result<bool, TlsError> {
        let cert = read(&self.cert_file)?;
        let key = read(&self.key_file)?;
        if digest(&[&cert, &key]) == self.current.load().digest {
            return Ok(false);
        }
        self.current.store(Arc::new(parse(&cert, &key)?));
        Ok(true)
    }

    /// Seconds from `now` (Unix) until the serving certificate expires;
    /// negative once it has.
    #[must_use]
    pub fn seconds_until_expiry(&self, now: i64) -> i64 {
        self.current.load().not_after_unix.saturating_sub(now)
    }

    /// The expiry of the serving certificate, in Unix seconds.
    #[must_use]
    pub fn not_after_unix(&self) -> i64 {
        self.current.load().not_after_unix
    }

    /// Reloads every `interval` until the process ends.
    pub fn spawn_reloader(self: &Arc<Self>, interval: Duration) -> tokio::task::JoinHandle<()> {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            ticker.tick().await;
            loop {
                ticker.tick().await;
                match this.reload() {
                    Ok(true) => tracing::info!(
                        not_after_unix = this.not_after_unix(),
                        "serving certificate reloaded"
                    ),
                    Ok(false) => {}
                    Err(error) => {
                        tracing::warn!(%error, "serving certificate reload failed; keeping the previous one");
                    }
                }
            }
        })
    }
}

impl ResolvesServerCert for ReloadingCertificate {
    fn resolve(&self, _: ClientHello<'_>) -> Option<Arc<CertifiedKey>> {
        Some(Arc::clone(&self.current.load().key))
    }
}

/// Builds a client-certificate verifier trusting every certificate in
/// `client_ca_pem`.
fn client_verifier(client_ca_pem: &[u8]) -> Result<Arc<dyn ClientCertVerifier>, TlsError> {
    let mut roots = RootCertStore::empty();
    for der in CertificateDer::pem_slice_iter(client_ca_pem) {
        let der = der.map_err(|error| TlsError(format!("client CA PEM: {error}")))?;
        roots
            .add(der)
            .map_err(|error| TlsError(format!("client CA: {error}")))?;
    }
    if roots.is_empty() {
        return Err(TlsError(
            "the client CA file holds no certificate".to_owned(),
        ));
    }
    let verifier: Arc<dyn ClientCertVerifier> = WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .map_err(|error| TlsError(format!("client verifier: {error}")))?;
    Ok(verifier)
}

struct LoadedCa {
    verifier: Arc<dyn ClientCertVerifier>,
    digest: [u8; 32],
}

/// The listener's client-certificate verifier, following its CA bundle file.
///
/// It checks every handshake against the verifier built from the bundle as
/// last loaded; [`ReloadingClientCa::reload`] builds a new one and swaps it
/// in atomically. A bundle that fails to load leaves the previous one in
/// place.
///
/// It advertises no accepted-CA hints in the handshake (the current bundle
/// cannot be borrowed across a swap); a client with one certificate sends it
/// anyway, which is every client of this service.
pub struct ReloadingClientCa {
    file: PathBuf,
    current: ArcSwap<LoadedCa>,
}

impl fmt::Debug for ReloadingClientCa {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReloadingClientCa")
            .field("file", &self.file)
            .finish_non_exhaustive()
    }
}

impl ReloadingClientCa {
    /// Loads the bundle; the process does not start without a usable one.
    ///
    /// # Errors
    /// [`TlsError`] for an unreadable bundle or one with no certificate.
    pub fn load(file: &Path) -> Result<Arc<Self>, TlsError> {
        let pem = read(file)?;
        Ok(Arc::new(Self {
            file: file.to_owned(),
            current: ArcSwap::from_pointee(LoadedCa {
                verifier: client_verifier(&pem)?,
                digest: digest(&[&pem]),
            }),
        }))
    }

    /// Re-reads the bundle and, when it changed, swaps a verifier built from
    /// it in atomically. `Ok(true)` when it swapped.
    ///
    /// # Errors
    /// [`TlsError`] when the bundle cannot be read or holds no usable
    /// certificate; the previous bundle keeps verifying.
    pub fn reload(&self) -> Result<bool, TlsError> {
        let pem = read(&self.file)?;
        let fingerprint = digest(&[&pem]);
        if fingerprint == self.current.load().digest {
            return Ok(false);
        }
        self.current.store(Arc::new(LoadedCa {
            verifier: client_verifier(&pem)?,
            digest: fingerprint,
        }));
        Ok(true)
    }

    /// Reloads every `interval` until the process ends.
    pub fn spawn_reloader(self: &Arc<Self>, interval: Duration) -> tokio::task::JoinHandle<()> {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            ticker.tick().await;
            loop {
                ticker.tick().await;
                match this.reload() {
                    Ok(true) => tracing::info!("client CA bundle reloaded"),
                    Ok(false) => {}
                    Err(error) => {
                        tracing::warn!(%error, "client CA bundle reload failed; keeping the previous one");
                    }
                }
            }
        })
    }
}

impl ClientCertVerifier for ReloadingClientCa {
    fn offer_client_auth(&self) -> bool {
        self.current.load().verifier.offer_client_auth()
    }

    fn client_auth_mandatory(&self) -> bool {
        self.current.load().verifier.client_auth_mandatory()
    }

    fn root_hint_subjects(&self) -> &[rustls::DistinguishedName] {
        &[]
    }

    fn verify_client_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        now: rustls::pki_types::UnixTime,
    ) -> Result<ClientCertVerified, rustls::Error> {
        self.current
            .load()
            .verifier
            .verify_client_cert(end_entity, intermediates, now)
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        self.current
            .load()
            .verifier
            .verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        self.current
            .load()
            .verifier
            .verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        self.current.load().verifier.supported_verify_schemes()
    }
}

/// The listener's rustls configuration: mutual TLS against `client_ca_pem`,
/// serving whatever `certificate` currently holds. The CA bundle is fixed;
/// use [`server_config_with_ca`] for one that follows its file.
///
/// # Errors
/// [`TlsError`] for a CA bundle with no usable certificate.
pub fn server_config(
    certificate: Arc<ReloadingCertificate>,
    client_ca_pem: &[u8],
) -> Result<Arc<ServerConfig>, TlsError> {
    Ok(server_config_with_verifier(
        certificate,
        client_verifier(client_ca_pem)?,
    ))
}

/// [`server_config`] with a client CA bundle that follows its file
/// ([`ReloadingClientCa`]).
#[must_use]
pub fn server_config_with_ca(
    certificate: Arc<ReloadingCertificate>,
    client_ca: Arc<ReloadingClientCa>,
) -> Arc<ServerConfig> {
    server_config_with_verifier(certificate, client_ca)
}

fn server_config_with_verifier(
    certificate: Arc<ReloadingCertificate>,
    verifier: Arc<dyn ClientCertVerifier>,
) -> Arc<ServerConfig> {
    let mut config = ServerConfig::builder()
        .with_client_cert_verifier(verifier)
        .with_cert_resolver(certificate);
    config.alpn_protocols = vec![b"h2".to_vec()];
    Arc::new(config)
}

/// Accepts TCP connections on `listener` and completes their TLS handshakes
/// in the background; the stream yields the established connections.
///
/// A handshake never blocks the accept loop, a slow or silent peer is cut
/// off after `HANDSHAKE_TIMEOUT` (10 s), and past `MAX_HANDSHAKES` (256) in flight a
/// new connection is dropped. A failed accept backs off (50 ms doubling to
/// 1 s) instead of spinning.
pub fn tls_incoming(
    listener: TcpListener,
    config: Arc<ServerConfig>,
) -> ReceiverStream<Result<TlsStream<TcpStream>, io::Error>> {
    let (sender, receiver) = mpsc::channel(64);
    let acceptor = TlsAcceptor::from(config);
    let handshakes = Arc::new(Semaphore::new(MAX_HANDSHAKES));
    tokio::spawn(async move {
        let mut backoff = Duration::from_millis(50);
        loop {
            let accepted = tokio::select! {
                () = sender.closed() => return,
                accepted = listener.accept() => accepted,
            };
            let (stream, peer) = match accepted {
                Ok(connection) => {
                    backoff = Duration::from_millis(50);
                    connection
                }
                Err(error) => {
                    tracing::warn!(%error, "accept failed");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(1));
                    continue;
                }
            };
            let Ok(permit) = Arc::clone(&handshakes).try_acquire_owned() else {
                tracing::warn!(%peer, "too many handshakes in flight; connection dropped");
                continue;
            };
            let _ = stream.set_nodelay(true);
            let acceptor = acceptor.clone();
            let sender = sender.clone();
            tokio::spawn(async move {
                let handshake = tokio::time::timeout(HANDSHAKE_TIMEOUT, acceptor.accept(stream));
                match handshake.await {
                    Ok(Ok(tls)) => {
                        drop(permit);
                        let _ = sender.send(Ok(tls)).await;
                    }
                    Ok(Err(error)) => tracing::debug!(%peer, %error, "tls handshake failed"),
                    Err(_) => tracing::debug!(%peer, "tls handshake timed out"),
                }
            });
        }
    });
    ReceiverStream::new(receiver)
}

/// How the channel to elitea-main's introspection service is built.
#[derive(Clone, Debug)]
pub struct IntrospectionChannel {
    /// `https://…` URL of elitea-main's private control listener.
    pub url: String,
    /// CA of elitea-main's server certificate.
    pub ca_file: PathBuf,
    /// This service's client certificate and key.
    pub cert_file: PathBuf,
    pub key_file: PathBuf,
    /// TLS server name, when not the URL's host.
    pub server_name: Option<String>,
    /// Connect timeout.
    pub timeout: Duration,
}

impl IntrospectionChannel {
    /// A lazily connecting channel carrying the files' current material,
    /// with the fingerprint of that material for [`spawn_channel_reloader`].
    ///
    /// # Errors
    /// [`TlsError`] for an unreadable file or an unusable URL.
    pub fn build(&self) -> Result<(Channel, Fingerprint), TlsError> {
        // One read of each file serves both the channel and its fingerprint.
        let ca = read(&self.ca_file)?;
        let cert = read(&self.cert_file)?;
        let key = read(&self.key_file)?;
        let mut tls = ClientTlsConfig::new()
            .ca_certificate(Certificate::from_pem(&ca))
            .identity(Identity::from_pem(&cert, &key));
        if let Some(name) = &self.server_name {
            tls = tls.domain_name(name.clone());
        }
        let channel = Endpoint::from_shared(self.url.clone())
            .map_err(|error| TlsError(format!("introspection URL: {error}")))?
            .tls_config(tls)
            .map_err(|error| TlsError(format!("introspection TLS: {error}")))?
            .connect_timeout(self.timeout)
            .connect_lazy();
        Ok((channel, digest(&[&ca, &cert, &key])))
    }
}

/// A digest of the files a channel was built from.
pub type Fingerprint = [u8; 32];

/// Rebuilds the introspection channel when its certificate files change from
/// what `built_from` (returned by [`IntrospectionChannel::build`]) describes.
pub fn spawn_channel_reloader(
    settings: IntrospectionChannel,
    introspector: Arc<GrpcIntrospector>,
    built_from: Fingerprint,
    interval: Duration,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut last = Some(built_from);
        let mut ticker = tokio::time::interval(interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        ticker.tick().await;
        loop {
            ticker.tick().await;
            match settings.build() {
                Ok((_, fingerprint)) if Some(fingerprint) == last => {}
                Ok((channel, fingerprint)) => {
                    introspector.replace_channel(channel);
                    last = Some(fingerprint);
                    tracing::info!("introspection client certificate reloaded");
                }
                Err(error) => {
                    tracing::warn!(%error, "introspection client certificate reload failed; keeping the previous channel");
                }
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{CertificateParams, DnType, KeyPair};
    use rustls::pki_types::ServerName;
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

    struct Pair {
        cert: String,
        key: String,
    }

    /// A self-signed certificate for `localhost` with a distinguishing CN.
    fn pair(common_name: &str) -> Pair {
        let key = KeyPair::generate().expect("key");
        let mut params = CertificateParams::new(vec!["localhost".to_owned()]).expect("params");
        params
            .distinguished_name
            .push(DnType::CommonName, common_name);
        let cert = params.self_signed(&key).expect("certificate");
        Pair {
            cert: cert.pem(),
            key: key.serialize_pem(),
        }
    }

    fn write(dir: &Path, name: &str, pair: &Pair) -> (PathBuf, PathBuf) {
        let cert = dir.join(format!("{name}.crt"));
        let key = dir.join(format!("{name}.key"));
        std::fs::write(&cert, &pair.cert).expect("write cert");
        std::fs::write(&key, &pair.key).expect("write key");
        (cert, key)
    }

    fn scratch(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "elitea-vector-tls-{name}-{}-{}",
            std::process::id(),
            crate::auth::unix_now()
        ));
        std::fs::create_dir_all(&dir).expect("scratch dir");
        dir
    }

    /// The DER of the certificate a client sees from a handshake with
    /// `resolver` (no client authentication).
    async fn presented(resolver: &Arc<ReloadingCertificate>, trusted: &Pair) -> Vec<u8> {
        crate::install_crypto_provider();
        let server = Arc::new(
            ServerConfig::builder()
                .with_no_client_auth()
                .with_cert_resolver(Arc::clone(resolver) as Arc<dyn ResolvesServerCert>),
        );
        let (client_io, server_io) = tokio::io::duplex(16 * 1024);
        let acceptor = TlsAcceptor::from(server);
        let serving = tokio::spawn(async move {
            let mut stream = acceptor.accept(server_io).await.expect("server handshake");
            let _ = stream.write_all(b"x").await;
            let _ = stream.shutdown().await;
        });
        let mut roots = RootCertStore::empty();
        for der in CertificateDer::pem_slice_iter(trusted.cert.as_bytes()) {
            roots.add(der.expect("pem")).expect("root");
        }
        let client = Arc::new(
            rustls::ClientConfig::builder()
                .with_root_certificates(roots)
                .with_no_client_auth(),
        );
        let stream = tokio_rustls::TlsConnector::from(client)
            .connect(ServerName::try_from("localhost").expect("name"), client_io)
            .await;
        let mut stream = stream.expect("client handshake");
        let der = stream
            .get_ref()
            .1
            .peer_certificates()
            .and_then(|chain| chain.first())
            .map(|der| der.as_ref().to_vec())
            .expect("peer certificate");
        let mut byte = [0_u8; 1];
        let _ = stream.read(&mut byte).await;
        serving.await.expect("server task");
        der
    }

    fn first_der(pair: &Pair) -> Vec<u8> {
        CertificateDer::pem_slice_iter(pair.cert.as_bytes())
            .next()
            .expect("one")
            .expect("pem")
            .as_ref()
            .to_vec()
    }

    #[tokio::test]
    async fn a_new_handshake_presents_the_rotated_certificate() {
        let dir = scratch("rotate");
        let old = pair("old");
        let (cert_file, key_file) = write(&dir, "serving", &old);
        let resolver = ReloadingCertificate::load(&cert_file, &key_file).expect("load");
        assert_eq!(presented(&resolver, &old).await, first_der(&old));
        // Nothing changed: nothing swapped.
        assert!(!resolver.reload().expect("reload"));

        let new = pair("new");
        write(&dir, "serving", &new);
        assert!(resolver.reload().expect("reload"));
        assert_eq!(presented(&resolver, &new).await, first_der(&new));
        assert_ne!(first_der(&old), first_der(&new));
    }

    #[tokio::test]
    async fn a_bad_rotation_keeps_the_previous_certificate() {
        let dir = scratch("bad");
        let old = pair("old");
        let (cert_file, key_file) = write(&dir, "serving", &old);
        let resolver = ReloadingCertificate::load(&cert_file, &key_file).expect("load");
        // A half-rotated pair: the new certificate with the old key.
        let new = pair("new");
        std::fs::write(&cert_file, &new.cert).expect("write");
        assert!(resolver.reload().is_err());
        assert_eq!(presented(&resolver, &old).await, first_der(&old));
        // Garbage is refused too.
        std::fs::write(&cert_file, "not a certificate").expect("write");
        assert!(resolver.reload().is_err());
        assert_eq!(presented(&resolver, &old).await, first_der(&old));
        // The completed rotation is picked up on a later tick.
        write(&dir, "serving", &new);
        assert!(resolver.reload().expect("reload"));
    }

    #[test]
    fn expiry_is_reported_relative_to_now() {
        let dir = scratch("expiry");
        let (cert_file, key_file) = write(&dir, "serving", &pair("expiry"));
        let resolver = ReloadingCertificate::load(&cert_file, &key_file).expect("load");
        let not_after = resolver.not_after_unix();
        assert!(resolver.seconds_until_expiry(crate::auth::unix_now()) > MIN_VALIDITY_SECONDS);
        assert_eq!(resolver.seconds_until_expiry(not_after - 1800), 1800);
        assert!(resolver.seconds_until_expiry(not_after - 1800) <= MIN_VALIDITY_SECONDS);
        assert!(resolver.seconds_until_expiry(not_after + 10) < 0);
    }

    /// A CA and a client certificate it signed.
    struct ClientPki {
        ca_pem: String,
        cert: CertificateDer<'static>,
        key: PrivateKeyDer<'static>,
    }

    fn client_pki(name: &str) -> ClientPki {
        let ca_key = KeyPair::generate().expect("ca key");
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
        ca_params
            .distinguished_name
            .push(DnType::CommonName, format!("ca-{name}"));
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::DigitalSignature,
        ];
        let ca_pem = ca_params.self_signed(&ca_key).expect("ca").pem();
        let issuer = rcgen::Issuer::new(ca_params, ca_key);
        let key = KeyPair::generate().expect("client key");
        let cert = CertificateParams::new(vec![format!("{name}.client")])
            .expect("params")
            .signed_by(&key, &issuer)
            .expect("client certificate");
        ClientPki {
            ca_pem,
            cert: cert.der().clone(),
            key: PrivateKeyDer::try_from(key.serialize_der()).expect("key der"),
        }
    }

    /// Whether the server accepts a fresh handshake from `client`.
    async fn handshake_accepted(
        server: &Arc<ServerConfig>,
        serving: &Pair,
        client: &ClientPki,
    ) -> bool {
        let (client_io, server_io) = tokio::io::duplex(16 * 1024);
        let acceptor = TlsAcceptor::from(Arc::clone(server));
        let accepting = tokio::spawn(async move { acceptor.accept(server_io).await.is_ok() });
        let mut roots = RootCertStore::empty();
        for der in CertificateDer::pem_slice_iter(serving.cert.as_bytes()) {
            roots.add(der.expect("pem")).expect("root");
        }
        let config = Arc::new(
            rustls::ClientConfig::builder()
                .with_root_certificates(roots)
                .with_client_auth_cert(vec![client.cert.clone()], client.key.clone_key())
                .expect("client auth"),
        );
        let connecting = tokio_rustls::TlsConnector::from(config)
            .connect(ServerName::try_from("localhost").expect("name"), client_io);
        // TLS 1.3 reports a refused client certificate to the client only on
        // its first read, so the verdict is the server's.
        let connected = connecting.await;
        if let Ok(mut stream) = connected {
            let mut byte = [0_u8; 1];
            let _ = stream.read(&mut byte).await;
        }
        accepting.await.expect("server task")
    }

    #[tokio::test]
    async fn a_rotated_client_ca_admits_new_clients_on_a_new_handshake() {
        crate::install_crypto_provider();
        let dir = scratch("ca");
        let serving = pair("serving");
        let (cert_file, key_file) = write(&dir, "serving", &serving);
        let certificate = ReloadingCertificate::load(&cert_file, &key_file).expect("load");
        let old = client_pki("old");
        let new = client_pki("new");
        let ca_file = dir.join("client-ca.crt");
        std::fs::write(&ca_file, &old.ca_pem).expect("write ca");
        let ca = ReloadingClientCa::load(&ca_file).expect("load ca");
        let server = server_config_with_ca(certificate, Arc::clone(&ca));

        assert!(handshake_accepted(&server, &serving, &old).await);
        assert!(
            !handshake_accepted(&server, &serving, &new).await,
            "a client of a CA that is not trusted yet"
        );
        assert!(!ca.reload().expect("nothing changed"));

        // A bad bundle keeps the old CA.
        std::fs::write(&ca_file, "not a certificate").expect("write");
        assert!(ca.reload().is_err());
        std::fs::write(&ca_file, "").expect("write");
        assert!(ca.reload().is_err());
        assert!(handshake_accepted(&server, &serving, &old).await);
        assert!(!handshake_accepted(&server, &serving, &new).await);

        // The overlap bundle (old + new) admits both on new handshakes.
        std::fs::write(&ca_file, format!("{}{}", old.ca_pem, new.ca_pem)).expect("write");
        assert!(ca.reload().expect("swapped"));
        assert!(handshake_accepted(&server, &serving, &new).await);
        assert!(handshake_accepted(&server, &serving, &old).await);

        // The cutover drops the old CA.
        std::fs::write(&ca_file, &new.ca_pem).expect("write");
        assert!(ca.reload().expect("swapped"));
        assert!(handshake_accepted(&server, &serving, &new).await);
        assert!(!handshake_accepted(&server, &serving, &old).await);
    }

    #[tokio::test]
    async fn the_client_ca_reloader_follows_its_file() {
        crate::install_crypto_provider();
        let dir = scratch("ca-reloader");
        let serving = pair("serving");
        let (cert_file, key_file) = write(&dir, "serving", &serving);
        let certificate = ReloadingCertificate::load(&cert_file, &key_file).expect("load");
        let old = client_pki("old");
        let new = client_pki("new");
        let ca_file = dir.join("client-ca.crt");
        std::fs::write(&ca_file, &old.ca_pem).expect("write ca");
        let ca = ReloadingClientCa::load(&ca_file).expect("load ca");
        let server = server_config_with_ca(certificate, Arc::clone(&ca));
        let reloader = ca.spawn_reloader(Duration::from_millis(50));
        std::fs::write(&ca_file, &new.ca_pem).expect("write ca");
        tokio::time::timeout(Duration::from_secs(10), async {
            while !handshake_accepted(&server, &serving, &new).await {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .expect("the rotated CA was never picked up");
        reloader.abort();
    }

    /// Accepts any client certificate: the test reads which one was sent.
    #[derive(Debug)]
    struct AnyClient;

    impl rustls::server::danger::ClientCertVerifier for AnyClient {
        fn root_hint_subjects(&self) -> &[rustls::DistinguishedName] {
            &[]
        }
        fn verify_client_cert(
            &self,
            _: &CertificateDer<'_>,
            _: &[CertificateDer<'_>],
            _: rustls::pki_types::UnixTime,
        ) -> Result<rustls::server::danger::ClientCertVerified, rustls::Error> {
            Ok(rustls::server::danger::ClientCertVerified::assertion())
        }
        fn verify_tls12_signature(
            &self,
            _: &[u8],
            _: &CertificateDer<'_>,
            _: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
        }
        fn verify_tls13_signature(
            &self,
            _: &[u8],
            _: &CertificateDer<'_>,
            _: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
        }
        fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
            rustls::crypto::ring::default_provider()
                .signature_verification_algorithms
                .supported_schemes()
        }
    }

    #[derive(Debug)]
    struct Fixed(Arc<CertifiedKey>);

    impl ResolvesServerCert for Fixed {
        fn resolve(&self, _: ClientHello<'_>) -> Option<Arc<CertifiedKey>> {
            Some(Arc::clone(&self.0))
        }
    }

    /// A TLS server that sends every client certificate it receives down
    /// the returned channel, then holds the connection open.
    async fn recording_server(
        server_cert_pem: &str,
        server_key_pem: &str,
    ) -> (u16, mpsc::UnboundedReceiver<Vec<u8>>) {
        let loaded = parse(server_cert_pem.as_bytes(), server_key_pem.as_bytes()).expect("server");
        let mut config = ServerConfig::builder()
            .with_client_cert_verifier(Arc::new(AnyClient))
            .with_cert_resolver(Arc::new(Fixed(loaded.key)));
        config.alpn_protocols = vec![b"h2".to_vec()];
        let acceptor = TlsAcceptor::from(Arc::new(config));
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let port = listener.local_addr().expect("address").port();
        let (seen_tx, seen_rx) = mpsc::unbounded_channel::<Vec<u8>>();
        tokio::spawn(async move {
            while let Ok((stream, _)) = listener.accept().await {
                let acceptor = acceptor.clone();
                let seen = seen_tx.clone();
                tokio::spawn(async move {
                    let Ok(tls) = acceptor.accept(stream).await else {
                        return;
                    };
                    if let Some(der) = tls
                        .get_ref()
                        .1
                        .peer_certificates()
                        .and_then(|chain| chain.first())
                    {
                        let _ = seen.send(der.as_ref().to_vec());
                    }
                    tokio::time::sleep(Duration::from_secs(5)).await;
                });
            }
        });
        (port, seen_rx)
    }

    /// The channel is rebuilt from the files: a server that records the
    /// client certificate sees the old one, then the new one.
    #[tokio::test]
    async fn the_introspection_channel_follows_its_client_certificate() {
        use crate::auth::Introspector as _;
        crate::install_crypto_provider();
        let dir = scratch("channel");
        // A CA signs the server's certificate and is the channel's trust anchor.
        let ca_key = KeyPair::generate().expect("ca key");
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::DigitalSignature,
        ];
        let ca_pem = ca_params.self_signed(&ca_key).expect("ca").pem();
        let issuer = rcgen::Issuer::new(ca_params, ca_key);
        let server_key = KeyPair::generate().expect("server key");
        let server_cert = CertificateParams::new(vec!["localhost".to_owned()])
            .expect("params")
            .signed_by(&server_key, &issuer)
            .expect("server certificate")
            .pem();
        let ca_file = dir.join("ca.crt");
        std::fs::write(&ca_file, ca_pem).expect("write ca");

        let old = pair("client-old");
        let (cert_file, key_file) = write(&dir, "client", &old);
        let (port, mut seen) = recording_server(&server_cert, &server_key.serialize_pem()).await;
        let settings = IntrospectionChannel {
            url: format!("https://localhost:{port}"),
            ca_file,
            cert_file,
            key_file,
            server_name: Some("localhost".to_owned()),
            timeout: Duration::from_secs(2),
        };
        let (channel, built_from) = settings.build().expect("channel");
        let introspector = Arc::new(GrpcIntrospector::new(channel, Duration::from_millis(500)));
        let _ = introspector.introspect("t").await;
        let first = tokio::time::timeout(Duration::from_secs(5), seen.recv())
            .await
            .expect("timeout")
            .expect("a connection");
        assert_eq!(first, first_der(&old));

        let new = pair("client-new");
        write(&dir, "client", &new);
        let reloader = spawn_channel_reloader(
            settings,
            Arc::clone(&introspector),
            built_from,
            Duration::from_millis(50),
        );
        // The reloader rebuilds the channel; a call then connects afresh.
        let wanted = first_der(&new);
        tokio::time::timeout(Duration::from_secs(10), async {
            loop {
                let _ = introspector.introspect("t").await;
                if let Ok(Some(der)) =
                    tokio::time::timeout(Duration::from_millis(300), seen.recv()).await
                    && der == wanted
                {
                    return;
                }
            }
        })
        .await
        .expect("the new client certificate was never presented");
        reloader.abort();
    }
}
