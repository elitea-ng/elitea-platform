use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use adk_rust::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use chrono::Utc;
use hmac::{Hmac, KeyInit, Mac};
use md5::Md5;
use ring::digest::{SHA256, digest};
use rustls::pki_types::ServerName;
use rustls::{ClientConfig, RootCertStore};
use serde_json::{Map, Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufStream};
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;
use zeroize::{Zeroize, Zeroizing};

use super::config::{SMTP_PORT, YagmailToolkitConfig};

pub(in crate::toolkits) const MAX_MAILBOX_BYTES: usize = 254;
pub(in crate::toolkits) const MAX_MESSAGE_CHARS: usize = 12_288;
pub(in crate::toolkits) const MAX_MESSAGE_BYTES: usize = 48 * 1_024;
pub(in crate::toolkits) const MAX_SUBJECT_CHARS: usize = 249;
pub(in crate::toolkits) const MAX_SUBJECT_BYTES: usize = 998;
pub(in crate::toolkits) const MAX_CC_RECIPIENTS: usize = 100;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const SEND_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_MIME_BYTES: usize = 512 * 1_024;
const MAX_RESPONSE_LINE_BYTES: usize = 8 * 1_024;
const MAX_RESPONSE_BYTES: usize = 64 * 1_024;
const MAX_RESPONSE_LINES: usize = 100;
const MAX_AUTH_CHALLENGE_BYTES: usize = 4 * 1_024;
const MAX_SMTP_COMMAND_LINE_BYTES: usize = 512;
const MAX_AUTH_RESPONSE_LINE_BYTES: usize = 12 * 1_024;
const MAX_RESULT_BYTES: usize = 64 * 1_024;
const CLIENT_IDENTITY: &str = "[127.0.0.1]";
const LOWER_HEX: &[u8; 16] = b"0123456789abcdef";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum YagmailClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    SenderRejected,
    RecipientsRejected,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
}

/// Bounded SMTP failure without host, mailbox, password, or server text.
pub(crate) struct YagmailClientError {
    code: YagmailClientErrorCode,
    retryable: bool,
}

impl YagmailClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> YagmailClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            YagmailClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "yagmail.configuration.invalid",
                "the Yagmail toolkit configuration is invalid",
            ),
            YagmailClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "yagmail.message.invalid",
                "the email message is invalid",
            ),
            YagmailClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "yagmail.authentication.failed",
                "SMTP authentication failed",
            ),
            YagmailClientErrorCode::SenderRejected => (
                ErrorCategory::InvalidInput,
                "yagmail.sender.rejected",
                "the SMTP server rejected the configured sender",
            ),
            YagmailClientErrorCode::RecipientsRejected => (
                ErrorCategory::InvalidInput,
                "yagmail.recipients.rejected",
                "the SMTP server rejected every recipient",
            ),
            YagmailClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "yagmail.timeout",
                "the SMTP operation timed out before email data was dispatched",
            ),
            YagmailClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "yagmail.unavailable",
                "the SMTP service is unavailable",
            ),
            YagmailClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "yagmail.response.invalid",
                "the SMTP service returned an invalid response",
            ),
            YagmailClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "yagmail.resource_exhausted",
                "the email message exceeds the approved limit",
            ),
            YagmailClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "yagmail.effect.unknown_outcome",
                "the email may have been sent; reconcile delivery before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(
        code: YagmailClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for YagmailClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("YagmailClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for YagmailClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            YagmailClientErrorCode::InvalidConfiguration => {
                "the Yagmail client configuration is invalid"
            }
            YagmailClientErrorCode::InvalidInput => "the email message is invalid",
            YagmailClientErrorCode::Authentication => "SMTP authentication failed",
            YagmailClientErrorCode::SenderRejected => "the SMTP sender was rejected",
            YagmailClientErrorCode::RecipientsRejected => "all SMTP recipients were rejected",
            YagmailClientErrorCode::Timeout => "the SMTP operation timed out",
            YagmailClientErrorCode::DependencyUnavailable => "the SMTP service is unavailable",
            YagmailClientErrorCode::InvalidResponse => {
                "the SMTP service returned an invalid response"
            }
            YagmailClientErrorCode::ResourceExhausted => {
                "the email message exceeds its approved limit"
            }
            YagmailClientErrorCode::UnknownOutcome => {
                "the email delivery outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for YagmailClientError {}

#[async_trait]
pub(in crate::toolkits) trait YagmailApi: Send + Sync {
    async fn send_gmail_message(
        &self,
        effect_id: &str,
        receiver: &str,
        message: &str,
        subject: &str,
        cc: &[String],
    ) -> Result<Value, YagmailClientError>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum SmtpChannelErrorCode {
    Timeout,
    Unavailable,
    InvalidResponse,
    ResourceExhausted,
}

pub(in crate::toolkits) struct SmtpChannelError {
    code: SmtpChannelErrorCode,
}

impl SmtpChannelError {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: SmtpChannelErrorCode) -> Self {
        Self { code }
    }
}

impl fmt::Debug for SmtpChannelError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SmtpChannelError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

#[async_trait]
pub(in crate::toolkits) trait SmtpChannel: Send {
    async fn write_all(&mut self, bytes: &[u8]) -> Result<(), SmtpChannelError>;
    async fn read_line(&mut self, limit: usize) -> Result<Vec<u8>, SmtpChannelError>;
}

#[async_trait]
pub(in crate::toolkits) trait SmtpConnector: Send + Sync {
    async fn connect(
        &self,
        host: &str,
        port: u16,
    ) -> Result<Box<dyn SmtpChannel>, SmtpChannelError>;
}

struct RustlsSmtpConnector {
    tls: Arc<ClientConfig>,
}

impl RustlsSmtpConnector {
    fn new() -> Result<Self, YagmailClientError> {
        let native = rustls_native_certs::load_native_certs();
        let mut roots = RootCertStore::empty();
        let (accepted, _) = roots.add_parsable_certificates(native.certs);
        if accepted == 0 {
            return Err(invalid_configuration());
        }
        let tls =
            ClientConfig::builder_with_provider(rustls::crypto::ring::default_provider().into())
                .with_safe_default_protocol_versions()
                .map_err(|_| invalid_configuration())?
                .with_root_certificates(roots)
                .with_no_client_auth();
        Ok(Self { tls: Arc::new(tls) })
    }
}

#[async_trait]
impl SmtpConnector for RustlsSmtpConnector {
    async fn connect(
        &self,
        host: &str,
        port: u16,
    ) -> Result<Box<dyn SmtpChannel>, SmtpChannelError> {
        let server_name = ServerName::try_from(host.to_owned()).map_err(|_| channel_invalid())?;
        let tcp = tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect((host, port)))
            .await
            .map_err(|_| channel_timeout())?
            .map_err(|_| channel_unavailable())?;
        tcp.set_nodelay(true).map_err(|_| channel_unavailable())?;
        let stream = tokio::time::timeout(
            CONNECT_TIMEOUT,
            TlsConnector::from(Arc::clone(&self.tls)).connect(server_name, tcp),
        )
        .await
        .map_err(|_| channel_timeout())?
        .map_err(|_| channel_unavailable())?;
        Ok(Box::new(RustlsSmtpChannel {
            stream: BufStream::new(stream),
        }))
    }
}

struct RustlsSmtpChannel {
    stream: BufStream<tokio_rustls::client::TlsStream<TcpStream>>,
}

#[async_trait]
impl SmtpChannel for RustlsSmtpChannel {
    async fn write_all(&mut self, bytes: &[u8]) -> Result<(), SmtpChannelError> {
        self.stream
            .write_all(bytes)
            .await
            .map_err(|_| channel_unavailable())?;
        self.stream.flush().await.map_err(|_| channel_unavailable())
    }

    async fn read_line(&mut self, limit: usize) -> Result<Vec<u8>, SmtpChannelError> {
        let mut line = Vec::with_capacity(limit.min(256));
        loop {
            let (take, complete) = {
                let available = self
                    .stream
                    .fill_buf()
                    .await
                    .map_err(|_| channel_unavailable())?;
                if available.is_empty() {
                    return Err(channel_unavailable());
                }
                let end = available
                    .iter()
                    .position(|byte| *byte == b'\n')
                    .map_or(available.len(), |position| position + 1);
                if end > limit.saturating_sub(line.len()) {
                    return Err(channel_resource_exhausted());
                }
                line.extend_from_slice(&available[..end]);
                (end, available.get(end.wrapping_sub(1)) == Some(&b'\n'))
            };
            self.stream.consume(take);
            if complete {
                return Ok(line);
            }
        }
    }
}

pub(crate) struct YagmailClient {
    config: YagmailToolkitConfig,
    connector: Arc<dyn SmtpConnector>,
}

impl YagmailClient {
    pub(crate) fn new(config: YagmailToolkitConfig) -> Result<Self, YagmailClientError> {
        Ok(Self {
            config,
            connector: Arc::new(RustlsSmtpConnector::new()?),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn with_connector(
        config: YagmailToolkitConfig,
        connector: Arc<dyn SmtpConnector>,
    ) -> Self {
        Self { config, connector }
    }
}

#[async_trait]
impl YagmailApi for YagmailClient {
    async fn send_gmail_message(
        &self,
        effect_id: &str,
        receiver: &str,
        message: &str,
        subject: &str,
        cc: &[String],
    ) -> Result<Value, YagmailClientError> {
        validate_message(receiver, message, subject, cc)?;
        let mime = build_mime_message(
            effect_id,
            self.config.username(),
            receiver,
            subject,
            message,
            cc,
        )?;
        let dispatched = Arc::new(AtomicBool::new(false));
        let operation = async {
            let mut channel = self
                .connector
                .connect(self.config.host(), SMTP_PORT)
                .await
                .map_err(|error| map_channel_error(&error, false))?;
            let receipt = send_session(
                channel.as_mut(),
                self.config.username(),
                self.config.password(),
                receiver,
                cc,
                &mime,
                Arc::clone(&dispatched),
            )
            .await?;
            Ok::<_, YagmailClientError>(receipt)
        };
        let receipt = tokio::time::timeout(SEND_TIMEOUT, operation)
            .await
            .map_err(|_| {
                if dispatched.load(Ordering::Acquire) {
                    unknown_outcome()
                } else {
                    timeout(true)
                }
            })??;
        receipt.into_value()
    }
}

struct SmtpReceipt {
    refused: Vec<RefusedRecipient>,
}

struct RefusedRecipient {
    recipient: String,
    code: u16,
}

impl SmtpReceipt {
    fn into_value(self) -> Result<Value, YagmailClientError> {
        if self.refused.is_empty() {
            return Ok(Value::Object(Map::new()));
        }
        let refused = self
            .refused
            .into_iter()
            .map(|entry| json!({"recipient": entry.recipient, "smtp_code": entry.code}))
            .collect::<Vec<_>>();
        let value = json!({"sent": true, "refused_recipients": refused});
        if serde_json::to_vec(&value)
            .map_err(|_| unknown_outcome())?
            .len()
            > MAX_RESULT_BYTES
        {
            return Err(unknown_outcome());
        }
        Ok(value)
    }
}

async fn send_session(
    channel: &mut dyn SmtpChannel,
    username: &str,
    password: &str,
    receiver: &str,
    cc: &[String],
    mime: &[u8],
    dispatched: Arc<AtomicBool>,
) -> Result<SmtpReceipt, YagmailClientError> {
    expect_code(&read_response(channel, false).await?, 220, false)?;
    write_command(
        channel,
        format!("EHLO {CLIENT_IDENTITY}\r\n").as_bytes(),
        false,
    )
    .await?;
    let ehlo = read_response(channel, false).await?;
    expect_code(&ehlo, 250, false)?;
    let features = EhloFeatures::parse(&ehlo);
    authenticate(channel, username, password, features).await?;

    let size = if features.contains(EhloFeatures::SIZE) {
        format!(" SIZE={}", mime.len())
    } else {
        String::new()
    };
    let mail = format!("MAIL FROM:<{username}>{size}\r\n");
    write_command(channel, mail.as_bytes(), false).await?;
    let mail_response = read_response(channel, false).await?;
    if mail_response.code != 250 {
        return Err(if is_transient(mail_response.code) {
            dependency_unavailable(true)
        } else {
            sender_rejected()
        });
    }

    let mut recipients = Vec::with_capacity(cc.len() + 1);
    recipients.push(receiver);
    recipients.extend(cc.iter().map(String::as_str));
    let mut refused = Vec::new();
    for recipient in recipients {
        let command = format!("RCPT TO:<{recipient}>\r\n");
        write_command(channel, command.as_bytes(), false).await?;
        let response = read_response(channel, false).await?;
        if !matches!(response.code, 250 | 251) {
            refused.push(RefusedRecipient {
                recipient: recipient.to_owned(),
                code: response.code,
            });
        }
    }
    if refused.len() == cc.len() + 1 {
        let _ = write_command(channel, b"RSET\r\n", false).await;
        return Err(recipients_rejected());
    }

    write_command(channel, b"DATA\r\n", false).await?;
    let data_ready = read_response(channel, false).await?;
    if data_ready.code != 354 {
        return Err(if is_transient(data_ready.code) {
            dependency_unavailable(true)
        } else {
            invalid_response()
        });
    }
    dispatched.store(true, Ordering::Release);
    let data = dot_stuff(mime)?;
    write_command(channel, &data, true).await?;
    let accepted = read_response(channel, true).await?;
    if accepted.code != 250 {
        return Err(unknown_outcome());
    }
    Ok(SmtpReceipt { refused })
}

#[derive(Clone, Copy)]
struct EhloFeatures(u8);

impl EhloFeatures {
    const CRAM_MD5: u8 = 1 << 0;
    const PLAIN: u8 = 1 << 1;
    const LOGIN: u8 = 1 << 2;
    const SIZE: u8 = 1 << 3;

    const fn contains(self, feature: u8) -> bool {
        self.0 & feature != 0
    }

    fn parse(response: &SmtpResponse) -> Self {
        let mut features = Self(0);
        for line in response.lines.iter().skip(1) {
            let upper = line.iter().map(u8::to_ascii_uppercase).collect::<Vec<_>>();
            let text = String::from_utf8_lossy(&upper);
            let trimmed = text.trim();
            if trimmed == "SIZE" || trimmed.starts_with("SIZE ") {
                features.0 |= Self::SIZE;
            }
            let mechanisms = trimmed
                .strip_prefix("AUTH ")
                .or_else(|| trimmed.strip_prefix("AUTH="));
            if let Some(mechanisms) = mechanisms {
                for mechanism in mechanisms.split_ascii_whitespace() {
                    match mechanism {
                        "CRAM-MD5" => features.0 |= Self::CRAM_MD5,
                        "PLAIN" => features.0 |= Self::PLAIN,
                        "LOGIN" => features.0 |= Self::LOGIN,
                        _ => {}
                    }
                }
            }
        }
        features
    }
}

async fn authenticate(
    channel: &mut dyn SmtpChannel,
    username: &str,
    password: &str,
    features: EhloFeatures,
) -> Result<(), YagmailClientError> {
    if features.contains(EhloFeatures::CRAM_MD5)
        && authenticate_cram_md5(channel, username, password).await?
    {
        return Ok(());
    }
    if features.contains(EhloFeatures::PLAIN)
        && authenticate_plain(channel, username, password).await?
    {
        return Ok(());
    }
    if features.contains(EhloFeatures::LOGIN)
        && authenticate_login(channel, username, password).await?
    {
        return Ok(());
    }
    Err(authentication())
}

async fn authenticate_cram_md5(
    channel: &mut dyn SmtpChannel,
    username: &str,
    password: &str,
) -> Result<bool, YagmailClientError> {
    write_command(channel, b"AUTH CRAM-MD5\r\n", false).await?;
    let challenge = read_response(channel, false).await?;
    if challenge.code != 334 {
        return auth_result(challenge.code);
    }
    let encoded = challenge.lines.first().ok_or_else(invalid_response)?;
    if encoded.len() > MAX_AUTH_CHALLENGE_BYTES {
        return Err(resource_exhausted());
    }
    let challenge = BASE64_STANDARD
        .decode(encoded)
        .map_err(|_| invalid_response())?;
    if challenge.len() > MAX_AUTH_CHALLENGE_BYTES {
        return Err(resource_exhausted());
    }
    let mut mac = Hmac::<Md5>::new_from_slice(password.as_bytes()).map_err(|_| authentication())?;
    mac.update(&challenge);
    let digest = mac.finalize().into_bytes();
    let mut response = Zeroizing::new(String::with_capacity(username.len() + 1 + digest.len() * 2));
    response.push_str(username);
    response.push(' ');
    for byte in digest {
        response.push(char::from(LOWER_HEX[usize::from(byte >> 4)]));
        response.push(char::from(LOWER_HEX[usize::from(byte & 0x0f)]));
    }
    let mut command = Zeroizing::new(BASE64_STANDARD.encode(response.as_bytes()));
    command.push_str("\r\n");
    write_command(channel, command.as_bytes(), false).await?;
    auth_result(read_response(channel, false).await?.code)
}

async fn authenticate_plain(
    channel: &mut dyn SmtpChannel,
    username: &str,
    password: &str,
) -> Result<bool, YagmailClientError> {
    let mut payload = Zeroizing::new(Vec::with_capacity(username.len() + password.len() + 2));
    payload.push(0);
    payload.extend_from_slice(username.as_bytes());
    payload.push(0);
    payload.extend_from_slice(password.as_bytes());
    let encoded = Zeroizing::new(BASE64_STANDARD.encode(payload.as_slice()));
    validate_auth_response_len(encoded.len())?;
    let with_initial = "AUTH PLAIN ".len() + encoded.len() + 2 <= MAX_SMTP_COMMAND_LINE_BYTES;
    if with_initial {
        let mut command = Zeroizing::new(String::from("AUTH PLAIN "));
        command.push_str(&encoded);
        command.push_str("\r\n");
        write_command(channel, command.as_bytes(), false).await?;
    } else {
        write_command(channel, b"AUTH PLAIN\r\n", false).await?;
    }
    let mut response = read_response(channel, false).await?;
    if response.code == 334 {
        let mut repeated = Zeroizing::new(encoded.to_string());
        repeated.push_str("\r\n");
        write_command(channel, repeated.as_bytes(), false).await?;
        response = read_response(channel, false).await?;
    }
    auth_result(response.code)
}

async fn authenticate_login(
    channel: &mut dyn SmtpChannel,
    username: &str,
    password: &str,
) -> Result<bool, YagmailClientError> {
    let encoded_username = Zeroizing::new(BASE64_STANDARD.encode(username.as_bytes()));
    validate_auth_response_len(encoded_username.len())?;
    let with_initial =
        "AUTH LOGIN ".len() + encoded_username.len() + 2 <= MAX_SMTP_COMMAND_LINE_BYTES;
    if with_initial {
        let mut command = Zeroizing::new(String::from("AUTH LOGIN "));
        command.push_str(&encoded_username);
        command.push_str("\r\n");
        write_command(channel, command.as_bytes(), false).await?;
    } else {
        write_command(channel, b"AUTH LOGIN\r\n", false).await?;
    }
    let mut response = read_response(channel, false).await?;
    if response.code != 334 {
        return auth_result(response.code);
    }
    if !with_initial {
        let mut username_response = Zeroizing::new(encoded_username.to_string());
        username_response.push_str("\r\n");
        write_command(channel, username_response.as_bytes(), false).await?;
        response = read_response(channel, false).await?;
        if response.code != 334 {
            return auth_result(response.code);
        }
    }
    let mut encoded_password = Zeroizing::new(BASE64_STANDARD.encode(password.as_bytes()));
    validate_auth_response_len(encoded_password.len())?;
    encoded_password.push_str("\r\n");
    write_command(channel, encoded_password.as_bytes(), false).await?;
    auth_result(read_response(channel, false).await?.code)
}

fn auth_result(code: u16) -> Result<bool, YagmailClientError> {
    match code {
        235 | 503 => Ok(true),
        code if is_transient(code) => Err(dependency_unavailable(true)),
        500..=599 => Ok(false),
        _ => Err(invalid_response()),
    }
}

fn validate_auth_response_len(encoded_len: usize) -> Result<(), YagmailClientError> {
    if encoded_len + 2 > MAX_AUTH_RESPONSE_LINE_BYTES {
        Err(resource_exhausted())
    } else {
        Ok(())
    }
}

struct SmtpResponse {
    code: u16,
    lines: Vec<Vec<u8>>,
}

async fn read_response(
    channel: &mut dyn SmtpChannel,
    dispatched: bool,
) -> Result<SmtpResponse, YagmailClientError> {
    let mut code = None;
    let mut lines = Vec::new();
    let mut total = 0usize;
    loop {
        if lines.len() >= MAX_RESPONSE_LINES {
            return Err(map_after_dispatch(resource_exhausted(), dispatched));
        }
        let line = channel
            .read_line(MAX_RESPONSE_LINE_BYTES)
            .await
            .map_err(|error| map_channel_error(&error, dispatched))?;
        total = total
            .checked_add(line.len())
            .ok_or_else(resource_exhausted)?;
        if total > MAX_RESPONSE_BYTES {
            return Err(map_after_dispatch(resource_exhausted(), dispatched));
        }
        let mut line = line.as_slice();
        if let Some(stripped) = line.strip_suffix(b"\n") {
            line = stripped;
        }
        if let Some(stripped) = line.strip_suffix(b"\r") {
            line = stripped;
        }
        if line.len() < 3 || !line[..3].iter().all(u8::is_ascii_digit) {
            return Err(map_after_dispatch(invalid_response(), dispatched));
        }
        let parsed = u16::from(line[0] - b'0') * 100
            + u16::from(line[1] - b'0') * 10
            + u16::from(line[2] - b'0');
        if code.is_some_and(|expected| expected != parsed) {
            return Err(map_after_dispatch(invalid_response(), dispatched));
        }
        code = Some(parsed);
        let continuation = match line.get(3) {
            None | Some(b' ') => false,
            Some(b'-') => true,
            Some(_) => return Err(map_after_dispatch(invalid_response(), dispatched)),
        };
        lines.push(line.get(4..).unwrap_or_default().to_vec());
        if !continuation {
            return Ok(SmtpResponse {
                code: code.ok_or_else(invalid_response)?,
                lines,
            });
        }
    }
}

async fn write_command(
    channel: &mut dyn SmtpChannel,
    bytes: &[u8],
    dispatched: bool,
) -> Result<(), YagmailClientError> {
    channel
        .write_all(bytes)
        .await
        .map_err(|error| map_channel_error(&error, dispatched))
}

fn expect_code(
    response: &SmtpResponse,
    expected: u16,
    dispatched: bool,
) -> Result<(), YagmailClientError> {
    if response.code == expected {
        Ok(())
    } else if dispatched {
        Err(unknown_outcome())
    } else if is_transient(response.code) {
        Err(dependency_unavailable(true))
    } else {
        Err(invalid_response())
    }
}

fn is_transient(code: u16) -> bool {
    (400..=499).contains(&code)
}

fn map_channel_error(source: &SmtpChannelError, dispatched: bool) -> YagmailClientError {
    if dispatched {
        return unknown_outcome();
    }
    match source.code {
        SmtpChannelErrorCode::Timeout => timeout(true),
        SmtpChannelErrorCode::Unavailable => dependency_unavailable(true),
        SmtpChannelErrorCode::InvalidResponse => invalid_response(),
        SmtpChannelErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

fn map_after_dispatch(error: YagmailClientError, dispatched: bool) -> YagmailClientError {
    if dispatched { unknown_outcome() } else { error }
}

pub(in crate::toolkits) fn validate_message(
    receiver: &str,
    message: &str,
    subject: &str,
    cc: &[String],
) -> Result<(), YagmailClientError> {
    validate_mailbox(receiver)?;
    if message.len() > MAX_MESSAGE_BYTES
        || message.chars().count() > MAX_MESSAGE_CHARS
        || subject.len() > MAX_SUBJECT_BYTES
        || subject.chars().count() > MAX_SUBJECT_CHARS
    {
        return Err(resource_exhausted());
    }
    if cc.len() > MAX_CC_RECIPIENTS {
        return Err(resource_exhausted());
    }
    validate_multiline_text(message)?;
    if subject.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid_input());
    }
    for recipient in cc {
        validate_mailbox(recipient)?;
    }
    Ok(())
}

fn validate_multiline_text(value: &str) -> Result<(), YagmailClientError> {
    if value.bytes().any(|byte| {
        byte == 0 || (byte.is_ascii_control() && !matches!(byte, b'\t' | b'\r' | b'\n'))
    }) {
        return Err(invalid_input());
    }
    Ok(())
}

pub(in crate::toolkits) fn validate_mailbox(value: &str) -> Result<(), YagmailClientError> {
    if value.is_empty() {
        return Err(invalid_input());
    }
    if value.len() > MAX_MAILBOX_BYTES {
        return Err(resource_exhausted());
    }
    if !value.is_ascii()
        || value
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte.is_ascii_whitespace())
    {
        return Err(invalid_input());
    }
    let separator = mailbox_separator(value.as_bytes()).ok_or_else(invalid_input)?;
    let (local, domain_with_at) = value.split_at(separator);
    let domain = domain_with_at.get(1..).ok_or_else(invalid_input)?;
    if local.is_empty() || local.len() > 64 || domain.is_empty() || domain.len() > 255 {
        return Err(invalid_input());
    }
    validate_local_part(local.as_bytes())?;
    validate_mail_domain(domain.as_bytes())
}

fn mailbox_separator(value: &[u8]) -> Option<usize> {
    let mut quoted = false;
    let mut escaped = false;
    let mut separator = None;
    for (index, byte) in value.iter().copied().enumerate() {
        if escaped {
            escaped = false;
        } else if quoted && byte == b'\\' {
            escaped = true;
        } else if byte == b'"' {
            quoted = !quoted;
        } else if !quoted && byte == b'@' {
            if separator.is_some() {
                return None;
            }
            separator = Some(index);
        }
    }
    if quoted || escaped { None } else { separator }
}

fn validate_local_part(local: &[u8]) -> Result<(), YagmailClientError> {
    if local.first() == Some(&b'"') {
        if local.last() != Some(&b'"') || local.len() < 2 {
            return Err(invalid_input());
        }
        let mut escaped = false;
        for byte in local[1..local.len() - 1].iter().copied() {
            if escaped {
                if !(33..=126).contains(&byte) {
                    return Err(invalid_input());
                }
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'"' || !(32..=126).contains(&byte) {
                return Err(invalid_input());
            }
        }
        return if escaped {
            Err(invalid_input())
        } else {
            Ok(())
        };
    }
    if local.first() == Some(&b'.')
        || local.last() == Some(&b'.')
        || local.windows(2).any(|pair| pair == b"..")
        || !local.iter().copied().all(is_atext_or_dot)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn is_atext_or_dot(byte: u8) -> bool {
    byte.is_ascii_alphanumeric()
        || matches!(
            byte,
            b'!' | b'#'
                | b'$'
                | b'%'
                | b'&'
                | b'\''
                | b'*'
                | b'+'
                | b'-'
                | b'/'
                | b'='
                | b'?'
                | b'^'
                | b'_'
                | b'`'
                | b'{'
                | b'|'
                | b'}'
                | b'~'
                | b'.'
        )
}

fn validate_mail_domain(domain: &[u8]) -> Result<(), YagmailClientError> {
    if domain.first() == Some(&b'[') {
        if domain.last() != Some(&b']')
            || domain.len() < 3
            || domain[1..domain.len() - 1]
                .iter()
                .any(|byte| !(33..=126).contains(byte) || matches!(byte, b'[' | b']' | b'\\'))
        {
            return Err(invalid_input());
        }
        return Ok(());
    }
    let text = std::str::from_utf8(domain).map_err(|_| invalid_input())?;
    if !text.eq_ignore_ascii_case("localhost") && !text.contains('.') {
        return Err(invalid_input());
    }
    if text.split('.').any(|label| {
        label.is_empty()
            || label.len() > 63
            || label.starts_with('-')
            || label.ends_with('-')
            || !label
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
    }) {
        return Err(invalid_input());
    }
    Ok(())
}

fn build_mime_message(
    effect_id: &str,
    username: &str,
    receiver: &str,
    subject: &str,
    message: &str,
    cc: &[String],
) -> Result<Vec<u8>, YagmailClientError> {
    let normalized = normalize_newlines(message);
    let html = html_alternative(&normalized)?;
    if effect_id.is_empty()
        || effect_id.len() > 1_024
        || effect_id.bytes().any(|byte| byte.is_ascii_control())
    {
        return Err(invalid_input());
    }
    let identity = message_identity(effect_id, username, receiver, subject, &normalized, cc);
    let boundary = format!("elitea-{identity}-alternative");
    let mut output = BoundedMime::new();
    output.push(format!("Date: {}\r\n", Utc::now().to_rfc2822()).as_bytes())?;
    if !subject.is_empty() {
        output.push(b"Subject: ")?;
        output.push(encoded_header(subject)?.as_bytes())?;
        output.push(b"\r\n")?;
    }
    output.push(format!("From: <{username}>\r\n").as_bytes())?;
    output.push(format!("To: <{receiver}>\r\n").as_bytes())?;
    if !cc.is_empty() {
        output.push(b"Cc: ")?;
        for (index, recipient) in cc.iter().enumerate() {
            output.push(format!("<{recipient}>").as_bytes())?;
            if index + 1 != cc.len() {
                output.push(b",\r\n ")?;
            }
        }
        output.push(b"\r\n")?;
    }
    output.push(format!("Message-ID: <{identity}@elitea>\r\n").as_bytes())?;
    output.push(b"MIME-Version: 1.0\r\n")?;
    output.push(
        format!("Content-Type: multipart/alternative; boundary=\"{boundary}\"\r\n\r\n").as_bytes(),
    )?;
    output.push(format!("--{boundary}\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n").as_bytes())?;
    output.push(&base64_lines(normalized.as_bytes()))?;
    output.push(format!("--{boundary}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n").as_bytes())?;
    output.push(&base64_lines(html.as_bytes()))?;
    output.push(format!("--{boundary}--\r\n").as_bytes())?;
    output.finish()
}

fn normalize_newlines(value: &str) -> String {
    value.replace("\r\n", "\n").replace('\r', "\n")
}

fn html_alternative(value: &str) -> Result<String, YagmailClientError> {
    let mut html = String::with_capacity(value.len().saturating_add(64));
    html.push_str("<!doctype html><html><body><div>");
    for character in value.chars() {
        let escaped = match character {
            '&' => "&amp;",
            '<' => "&lt;",
            '>' => "&gt;",
            '"' => "&quot;",
            '\'' => "&#39;",
            '\n' => "<br>",
            _ => {
                html.push(character);
                if html.len() > MAX_MIME_BYTES {
                    return Err(resource_exhausted());
                }
                continue;
            }
        };
        html.push_str(escaped);
        if html.len() > MAX_MIME_BYTES {
            return Err(resource_exhausted());
        }
    }
    html.push_str("</div></body></html>");
    Ok(html)
}

fn encoded_header(value: &str) -> Result<String, YagmailClientError> {
    if value.is_ascii() && value.len() <= 69 {
        return Ok(value.to_owned());
    }
    let mut result = String::new();
    let mut chunk = String::new();
    for character in value.chars() {
        if !chunk.is_empty() && chunk.len() + character.len_utf8() > 42 {
            append_encoded_word(&mut result, &chunk);
            chunk.zeroize();
        }
        chunk.push(character);
    }
    if !chunk.is_empty() {
        append_encoded_word(&mut result, &chunk);
        chunk.zeroize();
    }
    if result.len() > MAX_MIME_BYTES {
        return Err(resource_exhausted());
    }
    Ok(result)
}

fn append_encoded_word(result: &mut String, value: &str) {
    if !result.is_empty() {
        result.push_str("\r\n ");
    }
    result.push_str("=?UTF-8?B?");
    result.push_str(&BASE64_STANDARD.encode(value.as_bytes()));
    result.push_str("?=");
}

fn message_identity(
    effect_id: &str,
    username: &str,
    receiver: &str,
    subject: &str,
    message: &str,
    cc: &[String],
) -> String {
    let mut material = Zeroizing::new(Vec::with_capacity(
        effect_id.len()
            + username.len()
            + receiver.len()
            + subject.len()
            + message.len()
            + cc.iter().map(String::len).sum::<usize>()
            + cc.len()
            + 5,
    ));
    material.extend_from_slice(effect_id.as_bytes());
    material.push(0);
    material.extend_from_slice(username.as_bytes());
    material.push(0);
    material.extend_from_slice(receiver.as_bytes());
    material.push(0);
    material.extend_from_slice(subject.as_bytes());
    material.push(0);
    material.extend_from_slice(message.as_bytes());
    for recipient in cc {
        material.push(0);
        material.extend_from_slice(recipient.as_bytes());
    }
    let hash = digest(&SHA256, &material);
    let mut identity = String::with_capacity(32);
    for byte in hash.as_ref().iter().copied().take(16) {
        identity.push(char::from(LOWER_HEX[usize::from(byte >> 4)]));
        identity.push(char::from(LOWER_HEX[usize::from(byte & 0x0f)]));
    }
    identity
}

fn base64_lines(value: &[u8]) -> Vec<u8> {
    let encoded = BASE64_STANDARD.encode(value);
    let mut lines = Vec::with_capacity(encoded.len() + encoded.len() / 76 * 2 + 2);
    for chunk in encoded.as_bytes().chunks(76) {
        lines.extend_from_slice(chunk);
        lines.extend_from_slice(b"\r\n");
    }
    if encoded.is_empty() {
        lines.extend_from_slice(b"\r\n");
    }
    lines
}

struct BoundedMime {
    bytes: Vec<u8>,
}

impl BoundedMime {
    fn new() -> Self {
        Self {
            bytes: Vec::with_capacity(4 * 1_024),
        }
    }

    fn push(&mut self, bytes: &[u8]) -> Result<(), YagmailClientError> {
        if bytes.len() > MAX_MIME_BYTES.saturating_sub(self.bytes.len()) {
            return Err(resource_exhausted());
        }
        self.bytes.extend_from_slice(bytes);
        Ok(())
    }

    fn finish(self) -> Result<Vec<u8>, YagmailClientError> {
        if self
            .bytes
            .split(|byte| *byte == b'\n')
            .any(|line| line.strip_suffix(b"\r").unwrap_or(line).len() > MAX_SUBJECT_BYTES)
        {
            return Err(resource_exhausted());
        }
        Ok(self.bytes)
    }
}

fn dot_stuff(mime: &[u8]) -> Result<Vec<u8>, YagmailClientError> {
    if mime.len() > MAX_MIME_BYTES {
        return Err(resource_exhausted());
    }
    let dot_count = mime
        .iter()
        .enumerate()
        .filter(|(index, byte)| **byte == b'.' && (*index == 0 || mime[*index - 1] == b'\n'))
        .count();
    let capacity = mime
        .len()
        .checked_add(dot_count)
        .and_then(|value| value.checked_add(3))
        .ok_or_else(resource_exhausted)?;
    if capacity > MAX_MIME_BYTES + 3 {
        return Err(resource_exhausted());
    }
    let mut stuffed = Vec::with_capacity(capacity);
    let mut line_start = true;
    for byte in mime.iter().copied() {
        if line_start && byte == b'.' {
            stuffed.push(b'.');
        }
        stuffed.push(byte);
        line_start = byte == b'\n';
    }
    if !stuffed.ends_with(b"\r\n") {
        stuffed.extend_from_slice(b"\r\n");
    }
    stuffed.extend_from_slice(b".\r\n");
    Ok(stuffed)
}

const fn error(code: YagmailClientErrorCode, retryable: bool) -> YagmailClientError {
    YagmailClientError { code, retryable }
}

const fn invalid_configuration() -> YagmailClientError {
    error(YagmailClientErrorCode::InvalidConfiguration, false)
}

const fn invalid_input() -> YagmailClientError {
    error(YagmailClientErrorCode::InvalidInput, false)
}

const fn authentication() -> YagmailClientError {
    error(YagmailClientErrorCode::Authentication, false)
}

const fn sender_rejected() -> YagmailClientError {
    error(YagmailClientErrorCode::SenderRejected, false)
}

const fn recipients_rejected() -> YagmailClientError {
    error(YagmailClientErrorCode::RecipientsRejected, false)
}

const fn timeout(retryable: bool) -> YagmailClientError {
    error(YagmailClientErrorCode::Timeout, retryable)
}

const fn dependency_unavailable(retryable: bool) -> YagmailClientError {
    error(YagmailClientErrorCode::DependencyUnavailable, retryable)
}

const fn invalid_response() -> YagmailClientError {
    error(YagmailClientErrorCode::InvalidResponse, false)
}

const fn resource_exhausted() -> YagmailClientError {
    error(YagmailClientErrorCode::ResourceExhausted, false)
}

const fn unknown_outcome() -> YagmailClientError {
    error(YagmailClientErrorCode::UnknownOutcome, false)
}

const fn channel_timeout() -> SmtpChannelError {
    SmtpChannelError {
        code: SmtpChannelErrorCode::Timeout,
    }
}

const fn channel_unavailable() -> SmtpChannelError {
    SmtpChannelError {
        code: SmtpChannelErrorCode::Unavailable,
    }
}

const fn channel_invalid() -> SmtpChannelError {
    SmtpChannelError {
        code: SmtpChannelErrorCode::InvalidResponse,
    }
}

const fn channel_resource_exhausted() -> SmtpChannelError {
    SmtpChannelError {
        code: SmtpChannelErrorCode::ResourceExhausted,
    }
}
