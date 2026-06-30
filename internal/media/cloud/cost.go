package cloud

import (
	"fmt"
	"strings"

	"github.com/Niloen/nbackup/internal/media"
)

// A cloud medium prices itself: newCost (wired as the Spec's Cost factory in cloud.go)
// reads the bucket URL scheme to pick a provider rate table.

// providerRates holds the built-in list-price ESTIMATES per provider (US-region,
// standard/hot storage), in $/GiB-month, $/GiB egress, and $/1000 GET. The provider
// invoice remains authoritative; these exist so `nb plan` shows a monthly bill with
// no configuration. Override any rate per-medium via the config `cost:` block.
var providerRates = map[string]media.Cost{
	"aws-s3":        {Provider: "aws-s3", StoragePerGiBMonth: 0.023, EgressPerGiB: 0.09, GetPer1000: 0.0004},
	"gcs":           {Provider: "gcs", StoragePerGiBMonth: 0.020, EgressPerGiB: 0.12, GetPer1000: 0.0004},
	"azure-blob":    {Provider: "azure-blob", StoragePerGiBMonth: 0.018, EgressPerGiB: 0.087, GetPer1000: 0.004},
	"generic-cloud": {Provider: "generic-cloud", StoragePerGiBMonth: 0.020, EgressPerGiB: 0.09, GetPer1000: 0.0004},
}

// schemeProvider maps a bucket URL scheme to its default provider table, so the
// common case is zero-config. Unknown schemes (file:// and mem:// in tests, or an
// S3-compatible endpoint behind a custom URL) fall back to a generic cloud table.
var schemeProvider = map[string]string{
	"s3":     "aws-s3",
	"gs":     "gcs",
	"azblob": "azure-blob",
}

// newCost builds a cloud medium's pricing: the provider table inferred from its URL
// scheme (or named by a `provider` override), with any scalar rate overrides applied.
func newCost(opts media.Options) (media.Cost, error) {
	prov := opts.Get("provider")
	if prov == "" {
		if p, ok := schemeProvider[schemeOf(opts.Get("url"))]; ok {
			prov = p
		} else {
			prov = "generic-cloud"
		}
	}
	c, ok := providerRates[prov]
	if !ok {
		return media.Cost{}, fmt.Errorf("unknown cost provider %q (known: aws-s3, gcs, azure-blob, generic-cloud)", prov)
	}
	for _, o := range []struct {
		key string
		dst *float64
	}{
		{"storage_per_gb_month", &c.StoragePerGiBMonth},
		{"egress_per_gb", &c.EgressPerGiB},
		{"get_per_1000", &c.GetPer1000},
	} {
		v, set, err := media.ParseRate(opts.Get(o.key))
		if err != nil {
			return media.Cost{}, err
		}
		if set {
			*o.dst = v
		}
	}
	return c, nil
}

// schemeOf returns the URL scheme (the part before "://").
func schemeOf(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		return url[:i]
	}
	return ""
}
