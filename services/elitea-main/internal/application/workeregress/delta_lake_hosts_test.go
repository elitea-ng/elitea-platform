package workeregress

import (
	"slices"
	"testing"
)

// A Delta Lake credential names no URL: the run's one host is the S3 endpoint
// of its region, derived exactly as families/delta_lake/config.rs s3_endpoint
// does, so no user-supplied host reaches the allowlist.
func TestDeltaLakeToolkitHostIsTheRegionsS3Endpoint(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		want           []string
	}{
		{
			"s3_path",
			`{"delta_lake_configuration":{"aws_region":"us-east-1","s3_path":"s3://acme-data/tables/events"}}`,
			[]string{"s3.us-east-1.amazonaws.com"},
		},
		{
			"table_path wins, s3a scheme",
			`{"delta_lake_configuration":{"aws_region":"eu-west-2","s3_path":"s3://a-bucket/x","table_path":"s3a://other-bucket/t"}}`,
			[]string{"s3.eu-west-2.amazonaws.com"},
		},
		{
			"govcloud",
			`{"delta_lake_configuration":{"aws_region":"us-gov-west-1","table_path":"s3://gov-bucket/t"}}`,
			[]string{"s3.us-gov-west-1.amazonaws.com"},
		},
		{
			"china",
			`{"delta_lake_configuration":{"aws_region":"cn-north-1","table_path":"s3://cn-bucket/t"}}`,
			[]string{"s3.cn-north-1.amazonaws.com.cn"},
		},
		{
			"nested by its owner",
			`{"settings":{"delta_lake_configuration":{"aws_region":"ap-south-1","s3_path":"s3://acme-data/t"}}}`,
			[]string{"s3.ap-south-1.amazonaws.com"},
		},
		{"a region that is a host", `{"delta_lake_configuration":{"aws_region":"us-east-1.evil.test","s3_path":"s3://acme-data/t"}}`, nil},
		{"no region", `{"delta_lake_configuration":{"s3_path":"s3://acme-data/t"}}`, nil},
		{"a local path", `{"delta_lake_configuration":{"aws_region":"us-east-1","s3_path":"/var/data/t"}}`, nil},
		{"an https path", `{"delta_lake_configuration":{"aws_region":"us-east-1","s3_path":"https://evil.test/t"}}`, nil},
		{"a malformed bucket", `{"delta_lake_configuration":{"aws_region":"us-east-1","s3_path":"s3://AB/t"}}`, nil},
		{"no configuration", `{"selected_tools":[]}`, nil},
		{"not json", `not json`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts, err := HostsForToolkit("delta_lake", []byte(tc.settings))
			if tc.name == "not json" {
				if err == nil {
					t.Fatalf("unreadable settings must be refused, got %v", hosts)
				}
				return
			}
			if err != nil {
				t.Fatalf("hosts: %v", err)
			}
			if !slices.Equal(hosts, tc.want) {
				t.Fatalf("hosts = %v, want %v", hosts, tc.want)
			}
		})
	}
	// Another type with the same keys derives nothing.
	hosts, err := HostsForToolkit("jira", []byte(`{"delta_lake_configuration":{"aws_region":"us-east-1","s3_path":"s3://acme-data/t"}}`))
	if err != nil || len(hosts) != 0 {
		t.Fatalf("jira: %v %v", hosts, err)
	}
}
