package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Event is one dispatched server-sent event.
type Event struct {
	ID    string
	Event string
	Data  string
}

// EventReader parses a text/event-stream (WHATWG HTML §9.2.6): fields up to a
// blank line make one event, `data` lines join with "\n", comment lines start
// with ':', and one space after the colon is dropped.
type EventReader struct {
	scanner *bufio.Scanner
}

// NewEventReader reads events from r.
func NewEventReader(r io.Reader) *EventReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &EventReader{scanner: scanner}
}

// Next returns the next event that carries data. io.EOF ends the stream.
func (r *EventReader) Next() (Event, error) {
	var event Event
	var data []string
	hasData := false
	for r.scanner.Scan() {
		line := strings.TrimSuffix(r.scanner.Text(), "\r")
		if line == "" {
			if hasData {
				event.Data = strings.Join(data, "\n")
				return event, nil
			}
			event, data = Event{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			if !strings.ContainsRune(value, 0) {
				event.ID = value
			}
		case "event":
			event.Event = value
		case "data":
			data = append(data, value)
			hasData = true
		}
	}
	if err := r.scanner.Err(); err != nil {
		return Event{}, err
	}
	return Event{}, io.EOF
}

// Stream is an open event stream.
type Stream struct {
	*EventReader
	body io.ReadCloser
}

// Close drops the connection.
func (s *Stream) Close() error { return s.body.Close() }

// StreamError is a refused stream request.
type StreamError struct{ Response *Response }

func (e *StreamError) Error() string { return "event stream refused: " + e.Response.String() }

// OpenStream opens an SSE stream; a non-empty lastEventID resumes after it.
func (c *Client) OpenStream(ctx context.Context, pathOrURL, lastEventID string) (*Stream, error) {
	request, err := c.NewRequest(ctx, http.MethodGet, pathOrURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	// No client timeout on a stream: the context bounds it.
	streaming := *c.HTTP
	streaming.Timeout = 0
	response, err := streaming.Do(request)
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", pathOrURL, err)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, &StreamError{Response: &Response{Status: response.StatusCode, Header: response.Header, Body: raw}}
	}
	if mediaType := response.Header.Get("Content-Type"); !strings.HasPrefix(mediaType, "text/event-stream") {
		_ = response.Body.Close()
		return nil, fmt.Errorf("open stream %s: Content-Type %q is not text/event-stream", pathOrURL, mediaType)
	}
	return &Stream{EventReader: NewEventReader(response.Body), body: response.Body}, nil
}

// ErrStreamEnded is returned when the server closed a stream before the
// caller saw what it was waiting for.
var ErrStreamEnded = errors.New("event stream ended")
