package gdrive

import "github.com/Niloen/nbackup/internal/media"

// Google Drive prices storage through the account's Google One / Workspace plan, not per
// API request, and — unlike the object stores — Drive bills no egress or per-GET charge.
// So only storage carries a rate. The default is a rough Google One estimate (2 TB ≈
// $9.99/mo ≈ $0.005/GiB-month) so `nb plan` shows a monthly figure with no configuration;
// override any rate per-medium via the config `cost:` block (the provider invoice stays
// authoritative). newCost mirrors the cloud medium's override-overlay shape.
func newCost(opts media.Options) (media.Cost, error) {
	c := media.Cost{Provider: "google-drive", StoragePerGiBMonth: 0.005}
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
