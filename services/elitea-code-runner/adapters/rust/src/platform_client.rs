//! Credential-free Rust client. The image owns the exchange implementation.
use serde::de::{DeserializeSeed, Error as _, MapAccess, SeqAccess, Visitor};
use serde::Serialize;
use serde_json::{Map, Value, json};
use std::{cell::Cell, fmt, io, rc::Rc};

const REQUEST: usize = 262_144;
const REPLY: usize = 2_097_152;
const CHUNK: usize = 65_536;
const OBJECT: usize = 8_388_608;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlatformError {
    InvalidFrame,
    InvalidResource,
    Exhausted,
    NotFound,
    SharingDenied,
    AuthorizationDenied,
    AuthenticationDenied,
    ApprovalRequired,
    SensitiveRejected,
    DependencyUnavailable,
    UnsupportedOperation,
    RevisionConflict,
    UnknownEffect,
    Stopped,
    LeaseLost,
}
impl fmt::Display for PlatformError {
    fn fmt(&self, out: &mut fmt::Formatter<'_>) -> fmt::Result { out.write_str("Code platform operation refused") }
}
impl std::error::Error for PlatformError {}

pub trait Exchange {
    /// Observe once. A failed observation never authorizes a resend.
    fn exchange(&mut self, exact_frame: &[u8]) -> Result<Vec<u8>, PlatformError>;
}

#[derive(Clone, Copy, Debug)]
pub enum Operation {
    ApplicationList, ApplicationGet, ApplicationVersionGet, UserGet,
    ToolkitList, ToolkitCall, SecretRead, BucketExists, BucketCreate,
    ArtifactList, ArtifactHead, ArtifactRead, ArtifactReadChunk,
    ArtifactWriteBegin, ArtifactWriteChunk, ArtifactWriteCommit, ArtifactAppend, ArtifactDelete,
}
impl Operation {
    fn name(self) -> &'static str {
        match self {
            Self::ApplicationList => "application_list", Self::ApplicationGet => "application_get",
            Self::ApplicationVersionGet => "application_version_get", Self::UserGet => "user_get",
            Self::ToolkitList => "toolkit_list", Self::ToolkitCall => "toolkit_call", Self::SecretRead => "secret_read",
            Self::BucketExists => "bucket_exists", Self::BucketCreate => "bucket_create",
            Self::ArtifactList => "artifact_list", Self::ArtifactHead => "artifact_head", Self::ArtifactRead => "artifact_read",
            Self::ArtifactReadChunk => "artifact_read_chunk", Self::ArtifactWriteBegin => "artifact_write_begin",
            Self::ArtifactWriteChunk => "artifact_write_chunk", Self::ArtifactWriteCommit => "artifact_write_commit",
            Self::ArtifactAppend => "artifact_append", Self::ArtifactDelete => "artifact_delete",
        }
    }
}

struct Seed { depth: usize, values: Rc<Cell<usize>> }
impl<'de> DeserializeSeed<'de> for Seed {
    type Value = Value;
    fn deserialize<D: serde::Deserializer<'de>>(self, deserializer: D) -> Result<Value, D::Error> {
        let count = self.values.get();
        if self.depth > 32 || count >= 10_000 { return Err(D::Error::custom("bounded JSON refused")); }
        self.values.set(count + 1);
        deserializer.deserialize_any(self)
    }
}
impl<'de> Visitor<'de> for Seed {
    type Value = Value;
    fn expecting(&self, out: &mut fmt::Formatter<'_>) -> fmt::Result { out.write_str("bounded finite JSON") }
    fn visit_unit<E: serde::de::Error>(self) -> Result<Value,E> { Ok(Value::Null) }
    fn visit_bool<E: serde::de::Error>(self, v: bool) -> Result<Value,E> { Ok(Value::Bool(v)) }
    fn visit_i64<E: serde::de::Error>(self, v: i64) -> Result<Value,E> { Ok(v.into()) }
    fn visit_u64<E: serde::de::Error>(self, v: u64) -> Result<Value,E> { Ok(v.into()) }
    fn visit_f64<E: serde::de::Error>(self, v: f64) -> Result<Value,E> {
        serde_json::Number::from_f64(v).map(Value::Number).ok_or_else(|| E::custom("nonfinite JSON"))
    }
    fn visit_str<E: serde::de::Error>(self, v: &str) -> Result<Value,E> { Ok(Value::String(v.to_owned())) }
    fn visit_string<E: serde::de::Error>(self, v: String) -> Result<Value,E> { Ok(Value::String(v)) }
    fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Value,A::Error> {
        let mut result = Vec::new();
        while let Some(child) = seq.next_element_seed(Seed { depth:self.depth+1, values:Rc::clone(&self.values) })? { result.push(child); }
        Ok(Value::Array(result))
    }
    fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Value,A::Error> {
        let mut result = Map::new();
        while let Some(key) = map.next_key::<String>()? {
            if result.contains_key(&key) { return Err(A::Error::custom("duplicate JSON key")); }
            let child = map.next_value_seed(Seed { depth:self.depth+1, values:Rc::clone(&self.values) })?;
            result.insert(key,child);
        }
        Ok(Value::Object(result))
    }
}
fn bounded_json(bytes: &[u8]) -> Result<Value,PlatformError> {
    let mut deserializer = serde_json::Deserializer::from_slice(bytes);
    let value = Seed { depth:0, values:Rc::new(Cell::new(0)) }.deserialize(&mut deserializer).map_err(|_| PlatformError::InvalidFrame)?;
    deserializer.end().map_err(|_| PlatformError::InvalidFrame)?;
    Ok(value)
}

fn bounded_shape(value: &Value, depth: usize, remaining: &mut usize) -> Result<(),PlatformError> {
    if depth > 32 || *remaining == 0 { return Err(PlatformError::Exhausted); }
    *remaining -= 1;
    match value {
        Value::Array(items) => for item in items { bounded_shape(item,depth+1,remaining)?; },
        Value::Object(items) => for item in items.values() { bounded_shape(item,depth+1,remaining)?; },
        _ => {}
    }
    Ok(())
}
struct CappedHeader(Vec<u8>);
impl io::Write for CappedHeader {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > REQUEST.saturating_sub(self.0.len()) {
            return Err(io::Error::other("Code platform header exceeds its bound"));
        }
        self.0.extend_from_slice(bytes); Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> { Ok(()) }
}
#[derive(Serialize)]
struct RequestHeader<'a> { revision:u8, sequence:u64, operation:&'static str, resource:&'a Value, arguments:&'a Value }
fn encode_request(sequence:u64, operation:Operation, resource:&Value, arguments:&Value, payload:&[u8]) -> Result<Vec<u8>,PlatformError> {
    if payload.len()>CHUNK || !resource.is_object() || !arguments.is_object() { return Err(PlatformError::InvalidResource); }
    let mut remaining=10_000;
    bounded_shape(resource,1,&mut remaining)?; bounded_shape(arguments,1,&mut remaining)?;
    let mut header=CappedHeader(Vec::new());
    serde_json::to_writer(&mut header,&RequestHeader { revision:1,sequence,operation:operation.name(),resource,arguments }).map_err(|_| PlatformError::Exhausted)?;
    bounded_json(&header.0)?;
    let mut frame=Vec::with_capacity(8+header.0.len()+payload.len());
    frame.extend_from_slice(&(header.0.len() as u32).to_be_bytes()); frame.extend_from_slice(&(payload.len() as u32).to_be_bytes()); frame.extend_from_slice(&header.0); frame.extend_from_slice(payload);
    Ok(frame)
}
fn exact(value: &Value, keys: &[&str]) -> bool {
    value.as_object().is_some_and(|m| m.len() == keys.len() && keys.iter().all(|k| m.contains_key(*k)))
}
fn digest(value: &Value) -> bool {
    value.as_str().is_some_and(|s| s.len() == 64 && s.bytes().all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v)))
}
fn status(value: &Value) -> Result<(),PlatformError> {
    Err(match value.as_str() {
        Some("ok") => return Ok(()), Some("not_found") => PlatformError::NotFound,
        Some("sharing_denied") => PlatformError::SharingDenied, Some("authorization_denied") => PlatformError::AuthorizationDenied,
        Some("authentication_denied") => PlatformError::AuthenticationDenied, Some("approval_required") => PlatformError::ApprovalRequired,
        Some("sensitive_rejected") => PlatformError::SensitiveRejected, Some("dependency_unavailable") => PlatformError::DependencyUnavailable, Some("unsupported_operation") => PlatformError::UnsupportedOperation,
        Some("invalid_resource") => PlatformError::InvalidResource, Some("invalid_frame") => PlatformError::InvalidFrame,
        Some("resource_exhausted") => PlatformError::Exhausted, Some("revision_conflict") => PlatformError::RevisionConflict,
        Some("unknown_effect") => PlatformError::UnknownEffect, Some("stopped") => PlatformError::Stopped,
        Some("lease_lost") => PlatformError::LeaseLost, _ => return Err(PlatformError::InvalidFrame),
    })
}
fn decode_reply(frame: Vec<u8>, sequence: u64) -> Result<(Value,Vec<u8>),PlatformError> {
    if frame.len() < 8 || frame.len() > 8+REPLY+CHUNK { return Err(PlatformError::InvalidFrame); }
    let header = u32::from_be_bytes(frame[..4].try_into().map_err(|_| PlatformError::InvalidFrame)?) as usize;
    let body = u32::from_be_bytes(frame[4..8].try_into().map_err(|_| PlatformError::InvalidFrame)?) as usize;
    if header > REPLY || body > CHUNK || frame.len() != 8+header+body { return Err(PlatformError::InvalidFrame); }
    let reply = bounded_json(&frame[8..8+header]).map_err(|_|PlatformError::InvalidFrame)?;
    if !exact(&reply,&["revision","sequence","status","result","receipt"]) || reply["revision"].as_u64() != Some(1) || reply["sequence"].as_u64() != Some(sequence) { return Err(PlatformError::InvalidFrame); }
    let receipt = &reply["receipt"];
    if !receipt.is_null() && (!exact(receipt,&["effect_id","call_sha256","state"]) || !digest(&receipt["effect_id"]) || !digest(&receipt["call_sha256"]) || !matches!(receipt["state"].as_str(),Some("committed"|"uncertain"))) { return Err(PlatformError::InvalidFrame); }
    match status(&reply["status"]) {
        Ok(()) if receipt["state"] == "committed" => Ok((reply["result"].clone(),frame[8+header..].to_vec())),
        Ok(()) => Err(PlatformError::InvalidFrame),
        Err(_) if !reply["result"].is_null() || body != 0 => Err(PlatformError::InvalidFrame),
        Err(error) => Err(error),
    }
}

/// One original Code process owns this client. It cannot clone the sequence owner.
pub struct SandboxClient<E> { exchange:E, sequence:u64, maximum:u64, unknown:bool }
impl<E: Exchange> SandboxClient<E> {
    pub fn new(exchange:E, maximum:u64) -> Result<Self,PlatformError> {
        if !(1..=4096).contains(&maximum) { return Err(PlatformError::InvalidResource); }
        Ok(Self { exchange,sequence:0,maximum,unknown:false })
    }
    pub fn call(&mut self, operation:Operation, resource:Value, arguments:Value, payload:&[u8]) -> Result<(Value,Vec<u8>),PlatformError> {
        if self.unknown { return Err(PlatformError::UnknownEffect); }
        if self.sequence >= self.maximum || payload.len() > CHUNK { return Err(PlatformError::Exhausted); }
        if !resource.is_object() || !arguments.is_object() { return Err(PlatformError::InvalidResource); }
        let sequence = self.sequence+1;
        let frame=encode_request(sequence,operation,&resource,&arguments,payload)?;
        self.sequence = sequence;
        let response = match self.exchange.exchange(&frame) {
            Ok(frame) => decode_reply(frame,sequence), Err(_) => Err(PlatformError::UnknownEffect),
        };
        if matches!(response,Err(PlatformError::InvalidFrame|PlatformError::UnknownEffect|PlatformError::Stopped|PlatformError::LeaseLost)) { self.unknown = true; }
        response
    }
    pub fn get_private_project_secret(&mut self, name:&str, default:Option<Value>) -> Result<Value,PlatformError> {
        match self.call(Operation::SecretRead,json!({"kind":"secret","scope":"personal","name":name}),json!({}),&[]) {
            Ok((value,_)) => Ok(value), Err(PlatformError::NotFound) => default.ok_or(PlatformError::NotFound), Err(error) => Err(error),
        }
    }
    pub fn get_user_data(&mut self) -> Result<Value,PlatformError> { self.call(Operation::UserGet,json!({"kind":"current_user"}),json!({}),&[]).map(|(v,_)|v) }
    pub fn get_list_of_apps(&mut self,cursor:Option<&str>,limit:u8) -> Result<Value,PlatformError> { self.call(Operation::ApplicationList,json!({"kind":"application_catalog"}),json!({"cursor":cursor,"limit":limit}),&[]).map(|(v,_)|v) }
    pub fn get_app_details(&mut self,id:u32) -> Result<Value,PlatformError> { self.call(Operation::ApplicationGet,json!({"kind":"application","id":id.to_string()}),json!({}),&[]).map(|(v,_)|v) }
    pub fn get_app_version_details(&mut self,id:u32,version:u32) -> Result<Value,PlatformError> { self.call(Operation::ApplicationVersionGet,json!({"kind":"application_version","application_id":id.to_string(),"version_id":version.to_string()}),json!({}),&[]).map(|(v,_)|v) }
    pub fn get_mcp_toolkits(&mut self,cursor:Option<&str>,limit:u8) -> Result<Value,PlatformError> { self.call(Operation::ToolkitList,json!({"kind":"toolkit_catalog"}),json!({"cursor":cursor,"limit":limit}),&[]).map(|(v,_)|v) }
    pub fn mcp_tool_call(&mut self,id:u32,revision:&str,tool:&str,arguments:Value) -> Result<Value,PlatformError> { self.call(Operation::ToolkitCall,json!({"kind":"toolkit","id":id.to_string(),"revision":revision,"tool":tool}),arguments,&[]).map(|(v,_)|v) }
    pub fn unsecret(&mut self,name:&str) -> Result<Value,PlatformError> { self.call(Operation::SecretRead,json!({"kind":"secret","scope":"project","name":name}),json!({}),&[]).map(|(v,_)|v) }
    pub fn bucket_exists(&mut self,bucket:&str) -> Result<Value,PlatformError> { self.call(Operation::BucketExists,json!({"kind":"bucket","name":bucket}),json!({}),&[]).map(|(v,_)|v) }
    pub fn create_bucket(&mut self,bucket:&str) -> Result<Value,PlatformError> { self.call(Operation::BucketCreate,json!({"kind":"bucket","name":bucket}),json!({}),&[]).map(|(v,_)|v) }
    pub fn list_artifacts(&mut self,bucket:&str,prefix:&str,cursor:Option<&str>,limit:u8) -> Result<Value,PlatformError> { self.call(Operation::ArtifactList,json!({"kind":"bucket","name":bucket}),json!({"prefix":prefix,"cursor":cursor,"limit":limit}),&[]).map(|(v,_)|v) }
    pub fn artifact_head(&mut self,bucket:&str,name:&str) -> Result<Value,PlatformError> { self.call(Operation::ArtifactHead,json!({"kind":"artifact","bucket":bucket,"name":name}),json!({}),&[]).map(|(v,_)|v) }
    pub fn append_artifact(&mut self,bucket:&str,name:&str,text:&str,expected_version:&str) -> Result<Value,PlatformError> { self.call(Operation::ArtifactAppend,json!({"kind":"artifact","bucket":bucket,"name":name}),json!({"text":text,"expected_version":expected_version}),&[]).map(|(v,_)|v) }
    pub fn delete_artifact(&mut self,bucket:&str,name:&str,expected_version:&str) -> Result<Value,PlatformError> { self.call(Operation::ArtifactDelete,json!({"kind":"artifact","bucket":bucket,"name":name}),json!({"expected_version":expected_version}),&[]).map(|(v,_)|v) }
    fn invalid_observation<T>(&mut self) -> Result<T,PlatformError> { self.unknown=true; Err(PlatformError::UnknownEffect) }
    pub fn read_artifact(&mut self, bucket:&str, name:&str) -> Result<Vec<u8>,PlatformError> {
        let (info,first) = self.call(Operation::ArtifactRead,json!({"kind":"artifact","bucket":bucket,"name":name}),json!({}),&[])?;
        let Some(size) = info["bytes"].as_u64().filter(|v| *v <= OBJECT as u64).map(|v|v as usize) else { return self.invalid_observation(); };
        if !exact(&info,&["bytes","transfer","version"]) || !digest(&info["transfer"]) || info["version"].as_str().is_none_or(|v|v.is_empty() || v.len()>1024 || v.bytes().any(|c|c<32 || c==127)) || first.len() > size { return self.invalid_observation(); }
        let mut output = Vec::with_capacity(size); output.extend_from_slice(&first);
        while output.len() < size {
            let (_,chunk) = self.call(Operation::ArtifactReadChunk,json!({"kind":"artifact_transfer","id":info["transfer"]}),json!({"offset":output.len()}),&[])?;
            if chunk.is_empty() || chunk.len() > size-output.len() { return self.invalid_observation(); }
            output.extend_from_slice(&chunk);
        }
        Ok(output)
    }
    pub fn write_artifact(&mut self, bucket:&str, name:&str, bytes:&[u8]) -> Result<Value,PlatformError> {
        if bytes.len() > OBJECT { return Err(PlatformError::Exhausted); }
        let (info,_) = self.call(Operation::ArtifactWriteBegin,json!({"kind":"artifact","bucket":bucket,"name":name}),json!({"bytes":bytes.len()}),&[])?;
        if !exact(&info,&["transfer"]) || !digest(&info["transfer"]) { return self.invalid_observation(); }
        for (index,chunk) in bytes.chunks(CHUNK).enumerate() {
            self.call(Operation::ArtifactWriteChunk,json!({"kind":"artifact_transfer","id":info["transfer"]}),json!({"offset":index*CHUNK}),chunk)?;
        }
        self.call(Operation::ArtifactWriteCommit,json!({"kind":"artifact_transfer","id":info["transfer"]}),json!({}),&[]).map(|(value,_)| value)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    struct Observations {calls:usize,status:&'static str,broken:bool}
    impl Exchange for Observations {
        fn exchange(&mut self,frame:&[u8])->Result<Vec<u8>,PlatformError> {
            self.calls+=1;
            if self.broken {return Err(PlatformError::UnknownEffect);}
            let size=u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;
            let request:Value=serde_json::from_slice(&frame[8..8+size]).unwrap();
            let reply=json!({"revision":1,"sequence":request["sequence"],"status":self.status,"result":if self.status=="ok" {json!({"ready":true})} else {Value::Null},"receipt":if self.status=="ok" {json!({"effect_id":"a".repeat(64),"call_sha256":"b".repeat(64),"state":"committed"})} else {Value::Null}});
            let head=serde_json::to_vec(&reply).unwrap();let mut out=Vec::new();out.extend_from_slice(&(head.len() as u32).to_be_bytes());out.extend_from_slice(&0u32.to_be_bytes());out.extend(head);Ok(out)
        }
    }
    #[test]
    fn defaults_only_authorized_not_found_and_no_unknown_resend() {
        for (status,error) in [("not_found",PlatformError::NotFound),("sharing_denied",PlatformError::SharingDenied),("authorization_denied",PlatformError::AuthorizationDenied),("dependency_unavailable",PlatformError::DependencyUnavailable)] {
            let mut client=SandboxClient::new(Observations{calls:0,status,broken:false},4).unwrap();
            let actual=client.get_private_project_secret("same",Some(json!("default")));
            if error==PlatformError::NotFound {assert_eq!(actual.unwrap(),json!("default"));} else {assert_eq!(actual.unwrap_err(),error);}
        }
        let mut client=SandboxClient::new(Observations{calls:0,status:"ok",broken:true},4).unwrap();
        assert_eq!(client.get_user_data().unwrap_err(),PlatformError::UnknownEffect);
        assert_eq!(client.get_user_data().unwrap_err(),PlatformError::UnknownEffect);assert_eq!(client.exchange.calls,1);
    }
    #[test]
    fn bounded_encode_preserves_sequence_and_unsupported_is_typed() {
        let mut client=SandboxClient::new(Observations{calls:0,status:"unsupported_operation",broken:false},1).unwrap();
        assert!(client.call(Operation::UserGet,json!({"kind":"current_user"}),json!({"huge":"x".repeat(REQUEST+1)}),&[]).is_err());
        assert_eq!(client.sequence,0);assert_eq!(client.exchange.calls,0);
        assert_eq!(client.append_artifact("data","name","value","v1").unwrap_err(),PlatformError::UnsupportedOperation);assert_eq!(client.exchange.calls,1);
    }
    #[test]
    fn exact_binary_and_bounded_duplicate_depth_validation() {
        let payload=[0,255,128,1];let frame=encode_request(1,Operation::ArtifactWriteChunk,&json!({"kind":"artifact_transfer","id":"a".repeat(64)}),&json!({"offset":0}),&payload).unwrap();
        let head=u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;assert_eq!(&frame[8+head..],&payload);
        assert!(bounded_json(br#"{"a":1,"a":2}"#).is_err());
        assert!(bounded_json(format!("{}0{}","[".repeat(35),"]".repeat(35)).as_bytes()).is_err());
    }
}
