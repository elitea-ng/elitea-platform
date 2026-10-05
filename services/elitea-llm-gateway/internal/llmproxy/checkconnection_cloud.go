package llmproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// checkconnection_cloud.go adds the two ai_credentials types whose auth is not
// a bearer or api-key header: amazon_bedrock (AWS SigV4) and vertex_ai (a
// Google service-account token exchange). Legacy's LiteLLM test_connection
// covered all six credential types; these two were the last holdouts of the
// Go port (#319 shipped the other four).
//
// Both follow the same rules as their neighbours in checkconnection.go:
//
//   - the probe is a real, minimal, READ-ONLY provider call — a list, never a
//     completion, so a check never bills the tenant for inference;
//   - the credential comes from the caller's not-yet-saved form payload;
//   - every host the probe touches is resolved by dialTargets FIRST and is put
//     through the operator's egress allowlist by CheckConnection before any of
//     them is dialled, and is then dialled by the SSRF-guarded client
//     (newCheckConnectionProbeClient) which refuses private addresses;
//   - the answer that crosses the wire is the fixed reason vocabulary plus a
//     Detail that names FIELDS only — never a value, never a provider body.
//
// Neither type is self-hosted, so neither ever gets AllowPrivateNetwork: AWS
// and Google are public endpoints, and a "bedrock" credential that resolves to
// a private address is an SSRF attempt, not a deployment.
//
// AMBIENT IDENTITY IS NEVER USED. Both AWS's and Google's SDKs fall back to
// the pod's own identity when a credential is incomplete
// (config.LoadDefaultConfig, google.FindDefaultCredentials). That fallback is
// the exact trap account/account.go documents for Bedrock: it turns a dropped
// tenant credential into a request AWS bills to the platform. These probes
// therefore build credentials ONLY from the supplied fields, and refuse an
// incomplete payload before any call.

// checkConnectionAWSSigningService is the SigV4 service name for the Bedrock
// CONTROL plane (bedrock.*), which serves foundation-model listing. It is not
// the runtime service (bedrock-runtime.*), which serves inference — the
// credential probe deliberately never touches that one. Only the MODEL probe
// (checkconnection_model.go) does, with its own dial target. SigV4 signs both
// hosts with the same service name.
const checkConnectionAWSSigningService = "bedrock"

// vertexCheckScope is the OAuth scope Vertex AI requires, identical to the
// scope bifrost's own vertex provider requests.
const vertexCheckScope = "https://www.googleapis.com/auth/cloud-platform"

// vertexDefaultTokenURI is Google's OAuth 2.0 token endpoint, used when the
// service-account document does not name one (x/oauth2/google.JWTTokenURL).
const vertexDefaultTokenURI = "https://oauth2.googleapis.com/token"

// awsRegionPattern bounds aws_region_name to the shape of a real AWS region
// (us-east-1, eu-central-1, ap-southeast-3, us-gov-west-1, cn-north-1).
//
// This is a security control, not a nicety. The region is TENANT-AUTHORED and
// is interpolated into the host name, so an unvalidated value turns
// "https://bedrock.%s.amazonaws.com/foundation-models" into a request at a
// host of the tenant's choosing (a value containing "/", "@", ":" or "?"
// re-points the authority or the path). Constraining it to lowercase letters,
// digits and hyphens in the documented region shape closes that.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]{1,2}$`)

// vertexLocationPattern bounds vertex_location the same way, for the same
// reason: the location is interpolated into "%s-aiplatform.googleapis.com".
// It accepts a single-region location (us-central1, europe-west4,
// northamerica-northeast1, me-west1).
var vertexLocationPattern = regexp.MustCompile(`^[a-z]{2,}(?:-[a-z]+)*-[a-z]+[0-9]{1,2}$`)

// vertexGlobalLocations are the location values that do NOT produce a
// region-prefixed host. "global" and the multi-region pools "us"/"eu" all list
// publisher models on the plain Vertex API host — mirroring bifrost's own
// getVertexModelListingAPIHost, whose comment records that the multi-region
// prediction hosts reject publishers.models.list.
var vertexGlobalLocations = map[string]struct{}{
	"global": {},
	"us":     {},
	"eu":     {},
}

// --- amazon_bedrock -------------------------------------------------------

// bedrockCheckRegion validates the three fields a Bedrock credential must
// carry, and returns the region. The required set is IDENTICAL to the set
// account/account.go's buildKey requires before it will build a
// schemas.BedrockKeyConfig, so a credential this check passes is a credential
// the runtime will accept, and one it refuses is one the runtime would refuse
// too (issue #454).
func bedrockCheckRegion(req checkConnectionRequest) (string, error) {
	var missing []string
	if strings.TrimSpace(req.AWSAccessKeyID) == "" {
		missing = append(missing, "aws_access_key_id")
	}
	if strings.TrimSpace(req.AWSSecretAccessKey) == "" {
		missing = append(missing, "aws_secret_access_key")
	}
	region := strings.TrimSpace(req.AWSRegionName)
	if region == "" {
		missing = append(missing, "aws_region_name")
	}
	if len(missing) > 0 {
		return "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "this credential is missing: " + strings.Join(missing, ", "),
		}
	}
	if !awsRegionPattern.MatchString(region) {
		return "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "aws_region_name is not a valid AWS region name",
		}
	}
	return region, nil
}

// bedrockCheckBaseURL is the Bedrock control-plane origin for a region. Only
// a region that already passed awsRegionPattern reaches it.
func bedrockCheckBaseURL(region string) string {
	return "https://bedrock." + region + ".amazonaws.com"
}

// checkConnectionBedrockTargets names the single host the Bedrock probe
// dials. AWS's China partition (amazonaws.com.cn) is deliberately not derived
// here: its region names do not appear in this platform's catalogue, and a
// guessed second host would be a host the allowlist gate could not be written
// against.
func checkConnectionBedrockTargets(req checkConnectionRequest) ([]string, error) {
	region, err := bedrockCheckRegion(req)
	if err != nil {
		return nil, err
	}
	return []string{bedrockCheckBaseURL(region)}, nil
}

// probeBedrockFoundationModels validates an amazon_bedrock credential with a
// SigV4-signed GET bedrock.{region}.amazonaws.com/foundation-models.
//
// That call is the Bedrock control plane's ListFoundationModels operation —
// the same request bifrost's own bedrock provider makes for its list-models
// path (providers/bedrock: "List models endpoint uses the bedrock service
// (not bedrock-runtime)"). It is read-only, has no request body, returns the
// region's model catalogue, and bills nothing. It is the closest Bedrock
// analogue of the GET /models the open_ai checker uses.
//
// The signature is produced by the same signer bifrost signs its own Bedrock
// requests with (aws-sdk-go-v2's aws/signer/v4), over credentials built ONLY
// from the supplied fields — never through config.LoadDefaultConfig, which
// would silently fall back to the pod's IAM role and report the platform's
// own access as if it were the tenant's.
func probeBedrockFoundationModels(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	httpReq, err := signedBedrockControlPlaneRequest(ctx, req, "/foundation-models")
	if err != nil {
		return err
	}
	// Nothing may be added to the request after signing: any header the
	// signature covers would no longer match.
	return checkConnectionProbeDo(client, httpReq)
}

// signedBedrockControlPlaneRequest builds and SIGNS one read-only Bedrock
// control-plane GET for the credential's region.
//
// It is shared with the model-listing route (listprovidermodels.go), which
// makes the identical call and keeps the body. Sharing it rather than copying
// it is deliberate: a second copy of a SigV4 construction is a second place
// for the signed header set, the empty-payload hash and the "build credentials
// only from the supplied fields" rule to drift, and the last of those is the
// one that decides whether AWS bills the tenant or the platform.
//
// The returned request is COMPLETE. A caller must not touch its headers.
func signedBedrockControlPlaneRequest(
	ctx context.Context, req checkConnectionRequest, path string,
) (*http.Request, error) {
	region, err := bedrockCheckRegion(req)
	if err != nil {
		return nil, err
	}

	target := bedrockCheckBaseURL(region) + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &checkConnectionProbeError{err: err}
	}
	httpReq.Header.Set("Accept", "application/json")

	// SigV4 requires the hex SHA-256 of the payload. This request has none, so
	// it is the hash of the empty string. x-amz-content-sha256 is set for the
	// same reason bifrost sets it: required by some AWS services, harmless for
	// the rest, and it must be present BEFORE signing so the signature covers
	// it.
	emptyBody := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(emptyBody[:])
	httpReq.Header.Set("x-amz-content-sha256", payloadHash)

	creds := aws.Credentials{
		AccessKeyID:     strings.TrimSpace(req.AWSAccessKeyID),
		SecretAccessKey: req.AWSSecretAccessKey,
		SessionToken:    req.AWSSessionToken,
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, httpReq, payloadHash,
		checkConnectionAWSSigningService, region, time.Now().UTC()); err != nil {
		// A signing failure is a malformed credential, not an unreachable
		// provider: AWS never saw the request. Report it as a rejected
		// credential, with no detail derived from the key material.
		return nil, &checkConnectionProbeError{
			status: http.StatusUnauthorized,
			err:    fmt.Errorf("check_connection: could not sign the bedrock request: %w", err),
		}
	}
	return httpReq, nil
}

// --- vertex_ai ------------------------------------------------------------

// vertexServiceAccountDoc is the small part of the Google service-account
// document this checker needs to read for itself: the type (which decides
// whether the document is one this checker will use at all) and the token_uri
// (a host the probe will contact, so it has to be gated like any other).
type vertexServiceAccountDoc struct {
	Type     string `json:"type"`
	TokenURI string `json:"token_uri"`
}

// vertexCheckCredential validates a vertex_ai payload and returns the parsed
// service-account document plus the location.
//
// The required field set matches account/account.go's buildKey exactly
// (vertex_project, vertex_location, vertex_credentials), so this check accepts
// exactly the credentials the runtime accepts (issue #453). vertex_project is
// required for that parity even though the probe URL does not carry it: a
// credential without it is refused at dispatch, and a check that passed it
// would be the save-time false positive #319 exists to remove.
//
// ONLY a "service_account" document is checked. bifrost's runtime also accepts
// impersonated_service_account, authorized_user, external_account and
// external_account_authorized_user; those types name FURTHER endpoints inside
// the document (an impersonation URL, an external credential source), so the
// set of hosts the token exchange would contact cannot be enumerated here, and
// a host this file cannot enumerate is a host CheckConnection cannot put
// through the egress allowlist. Refusing to check them is honest; checking
// them through an ungated fetch would not be.
func vertexCheckCredential(req checkConnectionRequest) (vertexServiceAccountDoc, string, error) {
	var missing []string
	if strings.TrimSpace(req.VertexProject) == "" {
		missing = append(missing, "vertex_project")
	}
	location := strings.TrimSpace(req.VertexLocation)
	if location == "" {
		missing = append(missing, "vertex_location")
	}
	rawCredentials := strings.TrimSpace(string(req.VertexCredentials))
	if rawCredentials == "" {
		missing = append(missing, "vertex_credentials")
	}
	if len(missing) > 0 {
		return vertexServiceAccountDoc{}, "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "this credential is missing: " + strings.Join(missing, ", "),
		}
	}

	if _, global := vertexGlobalLocations[location]; !global && !vertexLocationPattern.MatchString(location) {
		return vertexServiceAccountDoc{}, "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "vertex_location is not a valid Vertex AI location",
		}
	}

	var doc vertexServiceAccountDoc
	if err := json.Unmarshal([]byte(rawCredentials), &doc); err != nil {
		// The parse error itself is never returned: it quotes the document,
		// and that document is the private key.
		return vertexServiceAccountDoc{}, "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "vertex_credentials is not a JSON document",
		}
	}
	if doc.Type != string(google.ServiceAccount) {
		return vertexServiceAccountDoc{}, "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "vertex_credentials must be a Google service-account key document",
		}
	}
	return doc, location, nil
}

// vertexCheckTokenURI returns the OAuth token endpoint the document names, or
// Google's default. It refuses anything that is not an absolute https URL:
// the token exchange sends a signed JWT assertion for the tenant's own service
// account, and http would put that assertion on the wire in the clear.
func vertexCheckTokenURI(doc vertexServiceAccountDoc) (string, error) {
	raw := strings.TrimSpace(doc.TokenURI)
	if raw == "" {
		return vertexDefaultTokenURI, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", &checkConnectionFieldError{
			reason: checkConnectionReasonMissingFields,
			detail: "vertex_credentials names a token_uri that is not an https URL",
		}
	}
	return raw, nil
}

// vertexCheckAPIHost returns the Vertex host that serves publisher-model
// listing for a location. It mirrors bifrost's getVertexModelListingAPIHost:
// "global" and the multi-region pools use the plain host, every other location
// uses the region-prefixed one.
func vertexCheckAPIHost(location string) string {
	if _, global := vertexGlobalLocations[location]; global {
		return "aiplatform.googleapis.com"
	}
	return location + "-aiplatform.googleapis.com"
}

// checkConnectionVertexTargets names BOTH hosts the Vertex probe contacts: the
// OAuth token endpoint and the Vertex API host. Returning both is what lets
// CheckConnection gate the token exchange as well as the API call — the token
// endpoint comes out of the tenant's own credential document, so leaving it
// ungated would be an SSRF hole the api_base gate never sees.
func checkConnectionVertexTargets(req checkConnectionRequest) ([]string, error) {
	doc, location, err := vertexCheckCredential(req)
	if err != nil {
		return nil, err
	}
	tokenURI, err := vertexCheckTokenURI(doc)
	if err != nil {
		return nil, err
	}
	return []string{"https://" + vertexCheckAPIHost(location), tokenURI}, nil
}

// probeVertexPublisherModels validates a vertex_ai credential in two steps:
//
//  1. mint an access token from the service-account document, over the
//     cloud-platform scope — the same exchange bifrost's vertex provider
//     performs (google.CredentialsFromJSONWithType, then TokenSource.Token);
//  2. call GET {host}/v1beta1/publishers/google/models?pageSize=1 with it —
//     the Model Garden listing endpoint bifrost's own Vertex list-models path
//     uses. It is read-only and bills nothing.
//
// Step 1 proves the key is a real, un-revoked service-account key that Google
// will still issue tokens for; step 2 proves Vertex accepts that token in the
// configured location. Between them they answer the question the button asks —
// "will this credential authenticate?" — with a real round trip, exactly like
// the other checkers.
//
// The token exchange is routed through the SAME SSRF-guarded client as the API
// call, by putting it on the context oauth2 reads its HTTP client from. Any
// other client would leave the one request whose destination comes out of the
// tenant's own document as the only unguarded call in this file.
func probeVertexPublisherModels(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	_, location, err := vertexCheckCredential(req)
	if err != nil {
		return err
	}
	accessToken, err := mintVertexAccessToken(ctx, client, req)
	if err != nil {
		return err
	}

	target := "https://" + vertexCheckAPIHost(location) + "/v1beta1/publishers/google/models?pageSize=1"
	return checkConnectionProbeGET(ctx, client, target, map[string]string{
		"Authorization": "Bearer " + accessToken,
	})
}

// mintVertexAccessToken performs step 1 of the Vertex probe: it validates the
// payload and exchanges the service-account assertion for an access token,
// over the SSRF-guarded client the caller supplies.
//
// It is shared with the model-listing route (listprovidermodels.go), which
// needs the same token for the same host. Sharing it rather than copying it
// keeps ONE place that decides which credential document types are accepted,
// which client the exchange is routed through, and how Google's refusal is
// classified — the last of which is the difference between telling an operator
// their key was revoked and telling them Google was unreachable.
//
// The token itself is returned to the caller and never logged.
func mintVertexAccessToken(ctx context.Context, client *http.Client, req checkConnectionRequest) (string, error) {
	doc, _, err := vertexCheckCredential(req)
	if err != nil {
		return "", err
	}
	if _, err := vertexCheckTokenURI(doc); err != nil {
		return "", err
	}

	authCtx := context.WithValue(ctx, oauth2.HTTPClient, client)
	creds, err := google.CredentialsFromJSONWithType(authCtx,
		[]byte(strings.TrimSpace(string(req.VertexCredentials))), google.ServiceAccount, vertexCheckScope)
	if err != nil {
		// The library's message quotes the document; it never crosses the wire.
		return "", &checkConnectionProbeError{
			status: http.StatusUnauthorized,
			err:    fmt.Errorf("check_connection: vertex_ai credentials could not be loaded: %w", err),
		}
	}

	token, err := creds.TokenSource.Token()
	if err != nil {
		// A *oauth2.RetrieveError means Google's token endpoint ANSWERED and
		// refused the assertion (it answers 400 invalid_grant for a revoked or
		// mistyped key, not 401), so this is a rejected credential and not an
		// unreachable provider. Anything else is a transport failure, which
		// classifies as unreachable through the zero status.
		var retrieveErr *oauth2.RetrieveError
		if errors.As(err, &retrieveErr) {
			return "", &checkConnectionProbeError{
				status: http.StatusUnauthorized,
				err:    fmt.Errorf("check_connection: google refused the service-account assertion (status %d)", retrieveErr.Response.StatusCode),
			}
		}
		return "", &checkConnectionProbeError{err: fmt.Errorf("check_connection: could not mint a google access token: %w", err)}
	}
	if token == nil || token.AccessToken == "" {
		return "", &checkConnectionProbeError{
			status: http.StatusUnauthorized,
			err:    fmt.Errorf("check_connection: google returned no access token"),
		}
	}
	return token.AccessToken, nil
}
