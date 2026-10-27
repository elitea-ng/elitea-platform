package storage

// The project `X-SECRET` value on the runtime-context token response (#408).
//
// The worker cannot read the project vault, so this route is the only channel
// that carries the value. Before it existed the SDK sent the literal "secret"
// on every call, and api/v2/applications had to accept that literal on any
// project whose vault held no value.
//
// The assertions below are on the RAW KEY SET, not on the decoded struct. Both
// workers refuse a response that carries a key they do not know — the Python
// worker compares the key set and the native worker parses with
// `deny_unknown_fields` — so "the field is absent" and "the field is empty" are
// different responses to them, and only the raw body tells the two apart.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func newSecretsHeaderTokenService(
	t *testing.T,
	secretsHeader ProjectSecretsHeaderReader,
) *EliteaClientTokenService {
	t.Helper()
	service, err := NewEliteaClientTokenService(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return validRuntimeContextAuthorization(), nil
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			return "header.payload.signature", nil
		}),
		projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			return validActorTokenPrincipal(), nil
		}),
		secretsHeader,
	)
	require.NoError(t, err)
	return service
}

// responseKeys reads the served object's keys, which is what a worker validates
// before it reads any value.
func responseKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &object))
	return object
}

func TestRuntimeContextServesTheProjectSecretsHeaderToAWorkerThatAsks(t *testing.T) {
	t.Parallel()

	const value = "Yk9tZS1yYW5kb20tcHJvamVjdC12YWx1ZQ"
	readProjects := []int64{}
	service := newSecretsHeaderTokenService(t, projectSecretsHeaderReaderFunc(
		func(_ context.Context, projectID int64) (string, error) {
			readProjects = append(readProjects, projectID)
			return value, nil
		}))
	server := newIndexRuntimeContextTestServer(t, service)

	request := validRuntimeContextRequest(t, runtimeContextFieldSecretsHeader)
	response := serve(server, request)

	require.Equal(t, codes.OK, response.Code)
	// The value is read for the CLAIM's project, not for one the request named.
	require.Equal(t, []int64{42}, readProjects)

	keys := responseKeys(t, response.Body)
	require.Contains(t, keys, "secrets_header_value")
	var decoded EliteaClientTokenContext
	require.NoError(t, json.Unmarshal(response.Body, &decoded))
	require.Equal(t, value, decoded.SecretsHeaderValue)
	require.Equal(t, "header.payload.signature", decoded.Token)
}

func TestRuntimeContextOmitsTheProjectSecretsHeaderWhenTheWorkerDoesNotAsk(t *testing.T) {
	t.Parallel()

	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("a-real-value"))
	server := newIndexRuntimeContextTestServer(t, service)

	// No accept header: a worker built before the field existed.
	request := validRuntimeContextRequest(t)
	response := serve(server, request)

	require.Equal(t, codes.OK, response.Code)
	keys := responseKeys(t, response.Body)
	// The exact three keys such a worker validates, and no fourth.
	require.Len(t, keys, 3)
	require.Contains(t, keys, "schema_version")
	require.Contains(t, keys, "project_id")
	require.Contains(t, keys, "token")
}

// A worker that did not ask (and every governed run, which never asks) has
// the project vault left unread, not read and then dropped.
func TestRuntimeContextDoesNotReadTheProjectSecretsHeaderWhenTheWorkerDoesNotAsk(t *testing.T) {
	t.Parallel()

	reads := 0
	service := newSecretsHeaderTokenService(t, projectSecretsHeaderReaderFunc(
		func(context.Context, int64) (string, error) {
			reads++
			return "a-real-value", nil
		}))
	server := newIndexRuntimeContextTestServer(t, service)

	response := serve(server, validRuntimeContextRequest(t))

	require.Equal(t, codes.OK, response.Code)
	require.Zero(t, reads, "the project X-SECRET value was read for a worker that did not accept it")
}

// A different field name in the accept list grants nothing. Without this, an
// accept header that was merely PRESENT would look like it worked.
func TestRuntimeContextOmitsTheProjectSecretsHeaderForAnotherAcceptedField(t *testing.T) {
	t.Parallel()

	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("a-real-value"))
	server := newIndexRuntimeContextTestServer(t, service)

	request := validRuntimeContextRequest(t, "some-other-field")
	response := serve(server, request)

	require.Equal(t, codes.OK, response.Code)
	require.Len(t, responseKeys(t, response.Body), 3)
}

func TestRuntimeContextAcceptListIsRead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "the exact name", values: []string{"secrets-header-value"}, want: true},
		{name: "a list", values: []string{"other", " secrets-header-value ", "more"}, want: true},
		{name: "case blind", values: []string{"Secrets-Header-Value"}, want: true},
		{name: "no fields", values: nil, want: false},
		{name: "an empty field", values: []string{""}, want: false},
		{name: "a prefix only", values: []string{"secrets-header"}, want: false},
		{name: "another name", values: []string{"secrets-header-values"}, want: false},
		// A comma-joined entry is read as a list, exactly like separate
		// entries, so neither caller shape silently loses a field.
		{name: "a comma-joined entry", values: []string{"other, secrets-header-value"}, want: true},
		{name: "a comma-joined entry without spaces", values: []string{"workspace,secrets-header-value"}, want: true},
		{name: "a comma-joined prefix only", values: []string{"other,secrets-header"}, want: false},
		{name: "only commas", values: []string{",,"}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, acceptsField(test.values, runtimeContextFieldSecretsHeader))
		})
	}
}

// A project with NO value serves the other three keys and stays usable.
//
// The turn keeps running: the token is the identity of the whole execution and
// this value authenticates one route. The route answers 403 with the repair,
// which is a lost sub-agent call rather than a lost conversation.
func TestRuntimeContextOmitsAnAbsentProjectSecretsHeader(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value string
		err   error
	}{
		{name: "the vault will not open", err: errors.New("decrypt p_42 vault key")},
		{name: "the project has no value", value: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := newSecretsHeaderTokenService(t, projectSecretsHeaderReaderFunc(
				func(context.Context, int64) (string, error) { return test.value, test.err }))
			server := newIndexRuntimeContextTestServer(t, service)

			request := validRuntimeContextRequest(t, runtimeContextFieldSecretsHeader)
			response := serve(server, request)

			require.Equal(t, codes.OK, response.Code)
			keys := responseKeys(t, response.Body)
			require.Len(t, keys, 3)
			require.Contains(t, keys, "token")
		})
	}
}

// The service refuses to be built without the reader. A nil dependency at the
// composition root is how this value would silently stop travelling.
func TestRuntimeContextTokenServiceRequiresTheSecretsHeaderReader(t *testing.T) {
	t.Parallel()

	authorizer := runtimeContextAuthorizerFunc(
		func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return validRuntimeContextAuthorization(), nil
		})
	issuer := actorTokenIssuerFunc(func(context.Context, int64) (string, error) { return "t", nil })
	validator := projectTokenValidatorFunc(
		func(context.Context, string) (auth.User, error) { return validActorTokenPrincipal(), nil })
	projectIssuer := projectSystemTokenIssuerFunc(
		func(context.Context, int64) (ProjectSystemToken, error) { return ProjectSystemToken{}, nil })

	_, err := NewEliteaClientTokenService(authorizer, issuer, validator, nil)
	require.Error(t, err)
	_, err = NewEliteaClientTokenServiceWithSchedules(authorizer, issuer, projectIssuer, validator, nil)
	require.Error(t, err)
}
