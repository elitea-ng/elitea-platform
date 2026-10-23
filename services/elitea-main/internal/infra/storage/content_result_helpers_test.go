package storage

import (
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// contentResult is one unary RuntimeContentService call as a test sees it:
// the status code, the JSON document of a successful call, and, for an
// attachment read, the named reason the object has no servable text.
type contentResult struct {
	Code       codes.Code
	Body       []byte
	Unreadable string
	Err        error
}

// contentCall is one prepared call, run against a server.
type contentCall func(*ContentServer) contentResult

func serve(server *ContentServer, call contentCall) contentResult {
	return call(server)
}

func resultOf(body []byte, err error) contentResult {
	if err != nil {
		// A refusal's Body is its status message, so an assertion that a
		// refusal names nothing private still has something to inspect.
		return contentResult{Code: grpcstatus.Code(err), Body: []byte(grpcstatus.Convert(err).Message()), Err: err}
	}
	return contentResult{Code: codes.OK, Body: body}
}
