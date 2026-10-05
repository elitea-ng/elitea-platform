//! A bounded Server-Sent Events decoder for chat-completion streams.
//!
//! The gateway is trusted to be the platform's, but what it relays comes
//! from a model provider; a stream that never ends a line, or one event
//! that grows without bound, must fail the call rather than grow the
//! engine's memory. So every buffer here has a cap, and the stream as a
//! whole has one.
//!
//! The subset of the SSE grammar a completion stream uses: lines end in
//! `\n`, `\r\n` or a lone `\r`; a UTF-8 byte order mark at the start of
//! the stream is skipped; `data:` lines of one event join with `\n`; a blank line
//! ends the event; `:` starts a comment (a keep-alive); `event:` names the
//! event (only `error` matters); `id:` and `retry:` are ignored. A trailing
//! event without its blank line is still delivered at the end of the
//! stream, because some servers close right after `data: [DONE]`.

/// The decoder's caps.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SseLimits {
    /// One line, terminator excluded.
    pub max_line_bytes: usize,
    /// One event's joined data.
    pub max_event_bytes: usize,
    /// The whole stream.
    pub max_stream_bytes: usize,
    /// Events in the stream. A 64k-token answer is about 64k events.
    pub max_events: usize,
}

impl Default for SseLimits {
    fn default() -> Self {
        Self {
            max_line_bytes: 1024 * 1024,
            max_event_bytes: 1024 * 1024,
            max_stream_bytes: 64 * 1024 * 1024,
            max_events: 200_000,
        }
    }
}

/// One event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SseEvent {
    /// The `event:` name, empty for the default `message`.
    pub event: String,
    pub data: String,
}

/// Why a stream was refused.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SseError {
    LineTooLong,
    EventTooLarge,
    StreamTooLarge,
    TooManyEvents,
    NotUtf8,
}

impl std::fmt::Display for SseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::LineTooLong => "the stream sent a line above the size cap",
            Self::EventTooLarge => "the stream sent an event above the size cap",
            Self::StreamTooLarge => "the stream exceeded its total size cap",
            Self::TooManyEvents => "the stream exceeded its event count cap",
            Self::NotUtf8 => "the stream sent bytes that are not UTF-8",
        })
    }
}

/// The incremental decoder.
#[derive(Debug, Default)]
pub struct SseDecoder {
    limits: SseLimits,
    pending: Vec<u8>,
    /// Bytes of `pending` already searched for a newline, so a long line
    /// fed in small chunks is scanned once, not once per chunk.
    scanned: usize,
    event: String,
    data: Vec<u8>,
    has_data: bool,
    total: usize,
    events: usize,
    /// The last line ended in `\r` at the end of a chunk: a `\n` that
    /// starts the next chunk is the second half of that `\r\n`.
    skip_lf: bool,
    /// The start of the stream was checked for a byte order mark.
    bom_checked: bool,
}

/// The UTF-8 byte order mark.
const BOM: &[u8] = b"\xEF\xBB\xBF";

impl SseDecoder {
    #[must_use]
    pub fn new(limits: SseLimits) -> Self {
        Self {
            limits,
            ..Self::default()
        }
    }

    /// Feed bytes; complete events are appended to `out`.
    ///
    /// # Errors
    ///
    /// The first cap the input crosses, or data that is not UTF-8.
    pub fn push(&mut self, chunk: &[u8], out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.total = self.total.saturating_add(chunk.len());
        if self.total > self.limits.max_stream_bytes {
            return Err(SseError::StreamTooLarge);
        }
        let mut chunk = chunk;
        if self.skip_lf && !chunk.is_empty() {
            self.skip_lf = false;
            chunk = chunk.strip_prefix(b"\n").unwrap_or(chunk);
        }
        self.pending.extend_from_slice(chunk);
        if !self.bom_checked {
            if self.pending.len() < BOM.len() && BOM.starts_with(&self.pending) {
                // Not enough bytes to tell yet.
                return Ok(());
            }
            self.bom_checked = true;
            if self.pending.starts_with(BOM) {
                self.pending.drain(..BOM.len());
            }
        }
        let mut start = 0;
        let mut from = self.scanned;
        while let Some(offset) = self.pending[from..]
            .iter()
            .position(|b| *b == b'\n' || *b == b'\r')
        {
            let end = from + offset;
            let line = self.pending[start..end].to_vec();
            start = end + 1;
            if self.pending[end] == b'\r' {
                match self.pending.get(start) {
                    Some(b'\n') => start += 1,
                    Some(_) => {}
                    None => self.skip_lf = true,
                }
            }
            from = start;
            if line.len() > self.limits.max_line_bytes {
                return Err(SseError::LineTooLong);
            }
            self.line(&line, out)?;
        }
        self.pending.drain(..start);
        self.scanned = self.pending.len();
        if self.pending.len() > self.limits.max_line_bytes {
            return Err(SseError::LineTooLong);
        }
        Ok(())
    }

    /// End of stream: deliver a last event that lacked its blank line.
    ///
    /// # Errors
    ///
    /// As [`SseDecoder::push`].
    pub fn finish(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.bom_checked = true;
        if !self.pending.is_empty() {
            let line = std::mem::take(&mut self.pending);
            self.scanned = 0;
            self.line(&line, out)?;
        }
        self.dispatch(out)
    }

    fn line(&mut self, line: &[u8], out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        if line.is_empty() {
            return self.dispatch(out);
        }
        if line[0] == b':' {
            return Ok(());
        }
        let (field, value) = match line.iter().position(|b| *b == b':') {
            Some(colon) => {
                let value = &line[colon + 1..];
                (&line[..colon], value.strip_prefix(b" ").unwrap_or(value))
            }
            None => (line, &[][..]),
        };
        match field {
            b"data" => {
                if self.has_data {
                    self.data.push(b'\n');
                }
                self.data.extend_from_slice(value);
                self.has_data = true;
                if self.data.len() > self.limits.max_event_bytes {
                    return Err(SseError::EventTooLarge);
                }
            }
            b"event" => {
                std::str::from_utf8(value)
                    .map_err(|_| SseError::NotUtf8)?
                    .clone_into(&mut self.event);
            }
            _ => {}
        }
        Ok(())
    }

    fn dispatch(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        let event = std::mem::take(&mut self.event);
        if !self.has_data {
            return Ok(());
        }
        self.has_data = false;
        let data =
            String::from_utf8(std::mem::take(&mut self.data)).map_err(|_| SseError::NotUtf8)?;
        self.events += 1;
        if self.events > self.limits.max_events {
            return Err(SseError::TooManyEvents);
        }
        out.push(SseEvent { event, data });
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decode(chunks: &[&[u8]], limits: SseLimits) -> Result<Vec<SseEvent>, SseError> {
        let mut decoder = SseDecoder::new(limits);
        let mut out = Vec::new();
        for chunk in chunks {
            decoder.push(chunk, &mut out)?;
        }
        decoder.finish(&mut out)?;
        Ok(out)
    }

    fn data(events: &[SseEvent]) -> Vec<&str> {
        events.iter().map(|e| e.data.as_str()).collect()
    }

    #[test]
    fn events_survive_any_chunking() {
        let stream = b": keep-alive\r\ndata: {\"a\":1}\r\n\r\ndata: x\ndata: y\n\nevent: error\ndata: boom\n\ndata: [DONE]\n\n";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let Ok(events) = decode(&[a, b], SseLimits::default()) else {
                panic!("split at {split} failed");
            };
            assert_eq!(data(&events), ["{\"a\":1}", "x\ny", "boom", "[DONE]"]);
            assert_eq!(events[2].event, "error");
            assert_eq!(events[3].event, "");
        }
    }

    #[test]
    fn a_last_event_without_its_blank_line_is_delivered() {
        let events = decode(&[b"data: [DONE]"], SseLimits::default());
        assert_eq!(events.map(|e| data(&e).join("|")), Ok("[DONE]".to_owned()));
    }

    #[test]
    fn the_caps_hold() {
        let small = SseLimits {
            max_line_bytes: 16,
            max_event_bytes: 15,
            max_stream_bytes: 64,
            max_events: 2,
        };
        assert_eq!(
            decode(&[b"data: 0123456789abcdefg\n\n"], small),
            Err(SseError::LineTooLong)
        );
        // A line that never ends is caught before the newline arrives.
        assert_eq!(
            decode(&[b"data: 0123456789abcdef"], small),
            Err(SseError::LineTooLong)
        );
        assert_eq!(
            decode(&[b"data: 0123456789\ndata: 0123456789\n\n"], small),
            Err(SseError::EventTooLarge)
        );
        assert_eq!(
            decode(&[b"data: a\n\ndata: b\n\ndata: c\n\n"], small),
            Err(SseError::TooManyEvents)
        );
        assert_eq!(
            decode(&[&[b':'; 65][..]], small),
            Err(SseError::StreamTooLarge)
        );
    }

    #[test]
    fn invalid_utf8_is_refused() {
        assert_eq!(
            decode(&[b"data: \xff\xfe\n\n"], SseLimits::default()),
            Err(SseError::NotUtf8)
        );
    }

    #[test]
    fn a_lone_carriage_return_ends_a_line() {
        let stream = b"data: a\r\rdata: b\r\n\r\ndata: c\r\r";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let Ok(events) = decode(&[a, b], SseLimits::default()) else {
                panic!("split at {split} failed");
            };
            assert_eq!(data(&events), ["a", "b", "c"], "split at {split}");
        }
        // A `\r\n` split across chunks is one line end, not two.
        let events = decode(
            &[b"data: a\r", b"\ndata: b\r", b"\n\r\n"],
            SseLimits::default(),
        );
        assert_eq!(events.map(|e| data(&e).join("|")), Ok("a\nb".to_owned()));
    }

    #[test]
    fn a_leading_byte_order_mark_is_skipped() {
        let stream = b"\xEF\xBB\xBFdata: x\n\n";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let events = decode(&[a, b], SseLimits::default());
            assert_eq!(
                events.map(|e| data(&e).join("|")),
                Ok("x".to_owned()),
                "split at {split}"
            );
        }
        // Only at the start of the stream.
        assert_eq!(
            decode(
                &[b"data: x\n\n\xEF\xBB\xBFdata: y\n\n"],
                SseLimits::default()
            )
            .map(|e| e.len()),
            Ok(1)
        );
    }
}
