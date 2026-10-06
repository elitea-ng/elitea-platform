//! Verifies a Main-owned committed reply before any byte reaches Code stdin.
use ring::signature;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io;

const DOMAIN: &[u8] = b"elitea.code.platform-committed-reply.ed25519.v1\0";
const MAX_HEADER: usize = 4096;
const MAX_REPLY: usize = 8 + 2_097_152 + 65_536;
fn refused() -> io::Error {
    io::Error::other("Code platform reply integrity refused")
}
fn hex<const N: usize>(value: &str) -> io::Result<[u8; N]> {
    if value.len() != N * 2 {
        return Err(refused());
    }
    let mut out = [0; N];
    for (index, pair) in value.as_bytes().chunks_exact(2).enumerate() {
        let nibble = |byte| match byte {
            b'0'..=b'9' => Ok(byte - b'0'),
            b'a'..=b'f' => Ok(byte - b'a' + 10),
            _ => Err(refused()),
        };
        out[index] = (nibble(pair[0])? << 4) | nibble(pair[1])?;
    }
    Ok(out)
}

/// Captured from the trusted fixed launch owner before the child exists.
/// No Code JSON field, argument, environment variable or mailbox chooses a key.
pub(crate) struct TrustedKeys(Vec<(String, [u8; 32])>);
impl TrustedKeys {
    pub(crate) fn new(keys: Vec<(String, [u8; 32])>) -> io::Result<Self> {
        if keys.is_empty() || keys.len() > 8 {
            return Err(refused());
        }
        for (index, (id, key)) in keys.iter().enumerate() {
            if id.is_empty()
                || id.len() > 256
                || !id.bytes().all(|v| (0x21..=0x7e).contains(&v))
                || *key == [0; 32]
                || keys[..index].iter().any(|(previous, _)| previous == id)
            {
                return Err(refused());
            }
        }
        Ok(Self(keys))
    }
    fn key(&self, id: &str) -> io::Result<&[u8; 32]> {
        self.0
            .iter()
            .find(|(key, _)| key == id)
            .map(|(_, value)| value)
            .ok_or_else(refused)
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Envelope<'a> {
    revision: u8,
    key_id: String,
    #[serde(borrow)]
    claims: &'a serde_json::value::RawValue,
    signature: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Claims {
    revision: u8,
    key_id: String,
    retained_runtime_id: String,
    prepared_sha256: String,
    policy_sha256: String,
    sequence: u64,
    request_frame_sha256: String,
    reply_frame_sha256: String,
    effect_id: String,
}
pub(crate) struct ExpectedReply<'a> {
    pub(crate) runtime_id: &'a str,
    pub(crate) prepared: &'a [u8; 32],
    pub(crate) policy: &'a [u8; 32],
    pub(crate) sequence: u64,
    pub(crate) request_sha256: &'a [u8; 32],
}
pub(crate) struct VerifiedReply<'a> {
    pub(crate) exact_frame: &'a [u8],
    // Retain the verified effect identity; adapters deliver only the exact frame.
    #[allow(dead_code)]
    pub(crate) effect_id: [u8; 32],
}

pub(crate) fn verify<'a>(
    frame: &'a [u8],
    expected: &ExpectedReply<'_>,
    keys: &TrustedKeys,
) -> io::Result<VerifiedReply<'a>> {
    if frame.len() < 8
        || frame.len() > 8 + MAX_HEADER + MAX_REPLY
        || expected.sequence < 1
        || expected.sequence > 4096
    {
        return Err(refused());
    }
    let header = u32::from_be_bytes(frame[..4].try_into().map_err(|_| refused())?) as usize;
    let body = u32::from_be_bytes(frame[4..8].try_into().map_err(|_| refused())?) as usize;
    if header == 0
        || header > MAX_HEADER
        || !(8..=MAX_REPLY).contains(&body)
        || frame.len() != 8 + header + body
    {
        return Err(refused());
    }
    let envelope: Envelope<'_> =
        serde_json::from_slice(&frame[8..8 + header]).map_err(|_| refused())?;
    if envelope.revision != 1 {
        return Err(refused());
    }
    let claims: Claims = serde_json::from_str(envelope.claims.get()).map_err(|_| refused())?;
    let exact_reply = &frame[8 + header..];
    let digest: [u8; 32] = Sha256::digest(exact_reply).into();
    if claims.revision != 1
        || claims.key_id != envelope.key_id
        || claims.retained_runtime_id != expected.runtime_id
        || hex::<32>(&claims.prepared_sha256)? != *expected.prepared
        || hex::<32>(&claims.policy_sha256)? != *expected.policy
        || claims.sequence != expected.sequence
        || hex::<32>(&claims.request_frame_sha256)? != *expected.request_sha256
        || hex::<32>(&claims.reply_frame_sha256)? != digest
    {
        return Err(refused());
    }
    let key = keys.key(&claims.key_id)?;
    let raw = envelope.claims.get().as_bytes();
    let mut input = Vec::with_capacity(DOMAIN.len() + 8 + raw.len());
    input.extend_from_slice(DOMAIN);
    input.extend_from_slice(&(raw.len() as u64).to_be_bytes());
    input.extend_from_slice(raw);
    signature::UnparsedPublicKey::new(&signature::ED25519, key)
        .verify(&input, &hex::<64>(&envelope.signature)?)
        .map_err(|_| refused())?;
    Ok(VerifiedReply {
        exact_frame: exact_reply,
        effect_id: hex::<32>(&claims.effect_id)?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::{
        rand::SystemRandom,
        signature::{Ed25519KeyPair, KeyPair},
    };
    use serde_json::json;
    fn lowerhex(bytes: &[u8]) -> String {
        use std::fmt::Write as _;
        bytes
            .iter()
            .fold(String::with_capacity(bytes.len() * 2), |mut out, byte| {
                write!(out, "{byte:02x}").unwrap();
                out
            })
    }
    fn signed(pair: &Ed25519KeyPair, claims: serde_json::Value, reply: &[u8]) -> Vec<u8> {
        let raw = serde_json::to_vec(&claims).unwrap();
        let mut input = DOMAIN.to_vec();
        input.extend_from_slice(&(raw.len() as u64).to_be_bytes());
        input.extend_from_slice(&raw);
        let header=serde_json::to_vec(&json!({"revision":1,"key_id":"main-v1","claims":claims,"signature":lowerhex(pair.sign(&input).as_ref())})).unwrap();
        let mut frame = Vec::new();
        frame.extend_from_slice(&(header.len() as u32).to_be_bytes());
        frame.extend_from_slice(&(reply.len() as u32).to_be_bytes());
        frame.extend_from_slice(&header);
        frame.extend_from_slice(reply);
        frame
    }
    #[test]
    fn accepts_exact_committed_reply_and_refuses_cross_binding_and_tampering() {
        let pkcs8 = Ed25519KeyPair::generate_pkcs8(&SystemRandom::new()).unwrap();
        let pair = Ed25519KeyPair::from_pkcs8(pkcs8.as_ref()).unwrap();
        let keys = TrustedKeys::new(vec![(
            "main-v1".into(),
            pair.public_key().as_ref().try_into().unwrap(),
        )])
        .unwrap();
        let reply = [3; 8];
        let expected = ExpectedReply {
            runtime_id: "original-pod-uid",
            prepared: &[1; 32],
            policy: &[2; 32],
            sequence: 7,
            request_sha256: &[4; 32],
        };
        let claims = json!({"revision":1,"key_id":"main-v1","retained_runtime_id":"original-pod-uid","prepared_sha256":lowerhex(&[1;32]),"policy_sha256":lowerhex(&[2;32]),"sequence":7,"request_frame_sha256":lowerhex(&[4;32]),"reply_frame_sha256":lowerhex(&Sha256::digest(reply)),"effect_id":lowerhex(&[5;32])});
        let frame = signed(&pair, claims.clone(), &reply);
        assert_eq!(verify(&frame, &expected, &keys).unwrap().exact_frame, reply);
        for (field, value) in [
            ("retained_runtime_id", json!("replacement-pod")),
            ("prepared_sha256", json!(lowerhex(&[6; 32]))),
            ("policy_sha256", json!(lowerhex(&[6; 32]))),
            ("sequence", json!(6)),
            ("request_frame_sha256", json!(lowerhex(&[6; 32]))),
            ("reply_frame_sha256", json!(lowerhex(&[6; 32]))),
        ] {
            let mut changed = claims.clone();
            changed[field] = value;
            assert!(verify(&signed(&pair, changed, &reply), &expected, &keys).is_err());
        }
        let mut corrupt = frame.clone();
        *corrupt.last_mut().unwrap() ^= 1;
        assert!(verify(&corrupt, &expected, &keys).is_err());
        for length in [0, 7, frame.len() - 1] {
            assert!(verify(&frame[..length], &expected, &keys).is_err());
        }
        let stale = TrustedKeys::new(vec![(
            "main-v0".into(),
            pair.public_key().as_ref().try_into().unwrap(),
        )])
        .unwrap();
        assert!(verify(&frame, &expected, &stale).is_err());
        let other_pkcs8 = Ed25519KeyPair::generate_pkcs8(&SystemRandom::new()).unwrap();
        let other = Ed25519KeyPair::from_pkcs8(other_pkcs8.as_ref()).unwrap();
        assert!(verify(&signed(&other, claims, &reply), &expected, &keys).is_err());
    }
}
