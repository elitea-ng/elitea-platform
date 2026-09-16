package pgvector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

// Every refusal below happens BEFORE a connection is attempted, which is the
// point: the target carries a DSN, so a writer that validated after connecting
// would turn a malformed request into an outbound connection to a project's
// vector store.
func TestCurrentIndexConfigurationWriterRefusesAnUnusableTargetWithoutConnecting(t *testing.T) {
	t.Parallel()

	valid := indexmetaapp.ResolvedTarget{
		// Deliberately unroutable: a guard that let one of these through would
		// hang on connect rather than fail the assertion, which is a visible
		// failure either way.
		ConnectionString: "postgresql://autotest:autotest@127.0.0.1:1/autotest",
		SchemaID:         19,
	}
	writer := NewCurrentIndexConfigurationWriter()

	for name, test := range map[string]struct {
		target        indexmetaapp.ResolvedTarget
		indexName     string
		configuration json.RawMessage
	}{
		"no schema":            {target: indexmetaapp.ResolvedTarget{ConnectionString: valid.ConnectionString}, indexName: "docs", configuration: json.RawMessage(`{}`)},
		"no dsn":               {target: indexmetaapp.ResolvedTarget{SchemaID: 19}, indexName: "docs", configuration: json.RawMessage(`{}`)},
		"newline in dsn":       {target: indexmetaapp.ResolvedTarget{ConnectionString: "postgresql://a\n@b/c", SchemaID: 19}, indexName: "docs", configuration: json.RawMessage(`{}`)},
		"no index name":        {target: valid, indexName: "", configuration: json.RawMessage(`{}`)},
		"newline in name":      {target: valid, indexName: "docs\nmore", configuration: json.RawMessage(`{}`)},
		"oversized index name": {target: valid, indexName: strings.Repeat("d", indexmetaapp.MaxCurrentIndexMetaCollectionBytes+1), configuration: json.RawMessage(`{}`)},
		"no configuration":     {target: valid, indexName: "docs"},
		"invalid json":         {target: valid, indexName: "docs", configuration: json.RawMessage(`{"a":`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := writer.SaveConfiguration(context.Background(), test.target, test.indexName, test.configuration)
			if err != ErrCurrentIndexMetaWrite {
				t.Fatalf("err=%v, want ErrCurrentIndexMetaWrite", err)
			}
		})
	}
}

func TestCurrentIndexConfigurationWriterRefusesANilReceiverAndACancelledContext(t *testing.T) {
	t.Parallel()

	var nilWriter *CurrentIndexConfigurationWriter
	target := indexmetaapp.ResolvedTarget{ConnectionString: "postgresql://a@b/c", SchemaID: 19}
	if err := nilWriter.SaveConfiguration(context.Background(), target, "docs", json.RawMessage(`{}`)); err != ErrCurrentIndexMetaWrite {
		t.Fatalf("nil receiver err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewCurrentIndexConfigurationWriter().SaveConfiguration(ctx, target, "docs", json.RawMessage(`{}`)); err != context.Canceled {
		t.Fatalf("cancelled err=%v", err)
	}
}
