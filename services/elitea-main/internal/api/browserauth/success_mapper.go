package browserauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"

	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

const (
	// MapperContractTrackedV1 identifies the complete checked-in auth_mappers
	// contract: header/grafana projects provider_attr.nameid to
	// X-WEBAUTH-USER; json/galloper projects it to login. Only the safe header
	// transport is enabled. JSON remains an evidence-only contract until its
	// consumer has workload-authenticated transport.
	MapperContractTrackedV1 = "elitea.auth_mappers.tracked.v1"

	headerMapperTarget = "header"
	grafanaScope       = "grafana"
	webAuthUserHeader  = "X-WEBAUTH-USER"
)

var ErrInvalidMapperConfiguration = errors.New("invalid authentication mapper configuration")

type MappedHeader struct {
	Name  string
	Value string
}

// MappingDisposition lets the HTTP boundary distinguish an unsupported
// projection from malformed data in an already-authorized browser session.
// The former is an ordinary access denial; the latter is a dependency contract
// failure and must not be disguised as a login/authorization failure.
type MappingDisposition uint8

const (
	MappingNotApplicable MappingDisposition = iota
	MappingApplied
	MappingInvalidAuthorizedData
)

// SuccessMapper is the fixed presentation adapter for the one safe projection
// evidenced in the tracked current configuration. It neither reopens a session
// nor accepts a separately supplied target/scope.
type SuccessMapper struct {
	headerName string
}

func NewSuccessMapper(contract string) (*SuccessMapper, error) {
	if contract != MapperContractTrackedV1 || !httpguts.ValidHeaderFieldName(webAuthUserHeader) {
		return nil, ErrInvalidMapperConfiguration
	}
	return &SuccessMapper{headerName: http.CanonicalHeaderKey(webAuthUserHeader)}, nil
}

func (mapper *SuccessMapper) Header(decision forwardapp.Decision) (MappedHeader, MappingDisposition) {
	if mapper == nil || !decision.Source.TargetPresent || decision.Source.Target != headerMapperTarget ||
		!decision.Source.ScopePresent || decision.Source.Scope != grafanaScope {
		return MappedHeader{}, MappingNotApplicable
	}
	authorization, ok := decision.AuthorizedBrowser()
	if !ok {
		if decision.Kind == forwardapp.DecisionAllow && decision.Reason == forwardapp.ReasonBrowserSessionAccepted {
			return MappedHeader{}, MappingInvalidAuthorizedData
		}
		return MappedHeader{}, MappingNotApplicable
	}
	providerReference, ok := providerReference(authorization.ProviderAttributes)
	if !ok || !httpguts.ValidHeaderFieldValue(providerReference) {
		return MappedHeader{}, MappingInvalidAuthorizedData
	}
	return MappedHeader{Name: mapper.headerName, Value: providerReference}, MappingApplied
}

func providerReference(attributes json.RawMessage) (string, bool) {
	if len(attributes) == 0 || len(attributes) > sessionstate.MaxProviderAttributesBytes || !utf8.Valid(attributes) {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(attributes))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", false
	}
	seen := make(map[string]struct{})
	var nameIDRaw json.RawMessage
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, keyOK := keyToken.(string)
		if err != nil || !keyOK {
			return "", false
		}
		if _, duplicate := seen[key]; duplicate {
			return "", false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", false
		}
		if key == "nameid" {
			nameIDRaw = append(nameIDRaw[:0], value...)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}

	var value string
	if len(nameIDRaw) == 0 || json.Unmarshal(nameIDRaw, &value) != nil || strings.TrimSpace(value) == "" ||
		value != strings.TrimSpace(value) ||
		len(value) > browserflow.MaxProviderReferenceBytes || !utf8.ValidString(value) ||
		strings.ContainsFunc(value, unicode.IsControl) {
		return "", false
	}
	return value, true
}
