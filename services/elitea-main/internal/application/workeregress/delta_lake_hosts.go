package workeregress

import (
	"bytes"
	"encoding/json"
	"net"
	"regexp"
	"strings"
)

// deltaLakeToolkitType is the worker's Delta Lake toolkit type
// (libs/rust/agent-runtime/src/toolkits/families/delta_lake).
const deltaLakeToolkitType = "delta_lake"

// deltaLakeRegion is the AWS region id shape the worker accepts
// (families/delta_lake/config.rs valid_region): a two-letter area, lowercase
// words, a one or two digit number. Pinned against the worker by
// endpoint_cases.json (delta_lake_hosts_test.go).
var deltaLakeRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)

// deltaLakeBucket is S3's bucket naming rule, as the worker checks it.
var deltaLakeBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// deltaLakeHosts answers the one host a Delta Lake toolkit calls: the S3
// endpoint of its credential's region. The credential names no URL, so the
// generic walk finds nothing; the worker derives `s3.<region>.amazonaws.com`
// (`.com.cn` for cn- regions) over HTTPS and puts the bucket in the path, so
// no user-supplied host ever reaches the allowlist. A credential the worker
// would refuse (no region, a malformed one, a table path that is not s3://)
// adds no host.
func deltaLakeHosts(content []byte) []string {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil
	}
	configuration := findDeltaLakeConfiguration(document, 0)
	if configuration == nil {
		return nil
	}
	region, _ := configuration["aws_region"].(string)
	region = strings.TrimSpace(region)
	if !deltaLakeRegion.MatchString(region) || len(region) > 25 {
		return nil
	}
	path := ""
	for _, key := range []string{"table_path", "s3_path"} {
		if value, _ := configuration[key].(string); strings.TrimSpace(value) != "" {
			path = strings.TrimSpace(value)
			break
		}
	}
	if !deltaLakeS3Path(path) {
		return nil
	}
	domain := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		domain = "amazonaws.com.cn"
	}
	host, ok := hostOf("https://s3." + region + "." + domain)
	if !ok {
		return nil
	}
	return []string{host}
}

// deltaLakeS3Path reports whether the path is an s3://, s3a:// or s3n:// URI
// with a bucket the worker accepts.
func deltaLakeS3Path(path string) bool {
	for _, r := range path {
		if r < ' ' || r == 0x7f || strings.ContainsRune("?#\\ ", r) {
			return false
		}
	}
	lower := strings.ToLower(path)
	for _, scheme := range []string{"s3://", "s3a://", "s3n://"} {
		if strings.HasPrefix(lower, scheme) {
			bucket, _, _ := strings.Cut(path[len(scheme):], "/")
			return deltaLakeBucket.MatchString(bucket) && !strings.Contains(bucket, "..") && net.ParseIP(bucket) == nil
		}
	}
	return false
}

// findDeltaLakeConfiguration finds the `delta_lake_configuration` object in a
// toolkit's settings (a flat settings document, or one nested by its owner).
func findDeltaLakeConfiguration(value any, depth int) map[string]any {
	if depth > maxWalkDepth {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if configuration, ok := object["delta_lake_configuration"].(map[string]any); ok {
		return configuration
	}
	for _, child := range object {
		if found := findDeltaLakeConfiguration(child, depth+1); found != nil {
			return found
		}
	}
	return nil
}
