package cloud

import (
	"testing"

	"github.com/Niloen/nbackup/internal/media"
)

func TestCostInferProvider(t *testing.T) {
	cases := []struct{ url, want string }{
		{"s3://bucket?region=us-east-1", "aws-s3"},
		{"gs://bucket", "gcs"},
		{"azblob://container", "azure-blob"},
		{"file:///tmp/bucket", "generic-cloud"},
		{"mem://", "generic-cloud"},
	}
	for _, c := range cases {
		got, err := newCost(media.Options{"url": c.url})
		if err != nil {
			t.Fatalf("newCost(%q): %v", c.url, err)
		}
		if got.Provider != c.want {
			t.Errorf("url %q -> provider %q, want %q", c.url, got.Provider, c.want)
		}
		if !got.Priced() {
			t.Errorf("url %q: cloud should be priced", c.url)
		}
	}
}

func TestCostProviderOverride(t *testing.T) {
	got, err := newCost(media.Options{"url": "file:///x", "provider": "aws-s3"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "aws-s3" || got.StoragePerGiBMonth != 0.023 {
		t.Errorf("provider override = %+v, want aws-s3 rates", got)
	}
}

func TestCostRateOverride(t *testing.T) {
	got, err := newCost(media.Options{"url": "s3://b", "egress_per_gb": "0.05", "storage_per_gb_month": "0.01"})
	if err != nil {
		t.Fatal(err)
	}
	if got.EgressPerGiB != 0.05 || got.StoragePerGiBMonth != 0.01 {
		t.Errorf("rate overrides not applied: %+v", got)
	}
	if got.GetPer1000 != 0.0004 {
		t.Errorf("un-overridden rate should keep the base value: %+v", got)
	}
}

func TestCostBadProvider(t *testing.T) {
	if _, err := newCost(media.Options{"url": "s3://b", "provider": "made-up"}); err == nil {
		t.Error("unknown provider should error")
	}
}

func TestCostBadRate(t *testing.T) {
	if _, err := newCost(media.Options{"url": "s3://b", "egress_per_gb": "cheap"}); err == nil {
		t.Error("non-numeric rate should error")
	}
}
