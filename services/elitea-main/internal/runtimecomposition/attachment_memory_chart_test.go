package runtimecomposition

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// attachmentProcessBaselineBytes is what elitea-main holds besides attachment
// extraction: the API, the pools, the caches.
const attachmentProcessBaselineBytes = 256 << 20

// TestChartMemoryLimitFitsAttachmentExtraction pins the relationship between
// the extractor's limits and the pod's memory limit. Before it, one hostile
// PDF or workbook could take the extractor past a 512 MiB limit, and the
// kernel then killed all of elitea-main, not only the extraction.
func TestChartMemoryLimitFitsAttachmentExtraction(t *testing.T) {
	t.Parallel()
	need := extract.PeakMemoryBytes(extract.DefaultLimits(), storage.MaxConcurrentAttachmentExtractions) +
		attachmentProcessBaselineBytes
	chart := filepath.Join("..", "..", "..", "..", "deploy", "helm", "elitea")
	files, err := filepath.Glob(filepath.Join(chart, "values*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no chart values files under %s: %v", chart, err)
	}
	checked := 0
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var values struct {
			Main struct {
				Resources struct {
					Limits struct {
						Memory string `yaml:"memory"`
					} `yaml:"limits"`
				} `yaml:"resources"`
			} `yaml:"main"`
		}
		if err := yaml.Unmarshal(content, &values); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		limit := values.Main.Resources.Limits.Memory
		if limit == "" {
			continue
		}
		checked++
		bytes, ok := memoryQuantityBytes(limit)
		if !ok {
			t.Fatalf("%s: main.resources.limits.memory %q is not a quantity this test reads", file, limit)
		}
		if bytes < need {
			t.Errorf("%s: main.resources.limits.memory %s (%d MiB) is below attachment extraction's peak plus the process baseline (%d MiB)",
				filepath.Base(file), limit, bytes>>20, need>>20)
		}
	}
	if checked == 0 {
		t.Fatal("no values file sets main.resources.limits.memory; the chart default moved")
	}
}

// memoryQuantityBytes reads the Kubernetes binary quantities a chart uses
// for memory (Ki, Mi, Gi) and plain bytes.
func memoryQuantityBytes(quantity string) (int64, bool) {
	for suffix, factor := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30} {
		if number, found := strings.CutSuffix(quantity, suffix); found {
			value, err := strconv.ParseInt(number, 10, 64)
			return value * factor, err == nil
		}
	}
	value, err := strconv.ParseInt(quantity, 10, 64)
	return value, err == nil
}

func TestMemoryQuantityBytes(t *testing.T) {
	t.Parallel()
	for quantity, want := range map[string]int64{"512Mi": 512 << 20, "1Gi": 1 << 30, "2048Ki": 2 << 20, "1000": 1000} {
		got, ok := memoryQuantityBytes(quantity)
		if !ok || got != want {
			t.Errorf("%s = %d %v, want %d", quantity, got, ok, want)
		}
	}
	if _, ok := memoryQuantityBytes("1G"); ok {
		t.Error("a decimal quantity must not read silently")
	}
}
