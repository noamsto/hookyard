package attest

import (
	"path/filepath"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
)

// VerdictRoot holds each store's verdict file. Set by -ldflags -X.
var VerdictRoot = "/var/lib/priors"

// VerifyUID is the decimal uid of the verdict job's account, the only owner a
// verdict file may have. Set by -ldflags -X; empty fails check 1, so an
// unpinned build never matches any owner.
var VerifyUID string

// Claims reports whether f claims review, which only an attestation grants.
func Claims(f fact.Fact) bool { return f.Metadata.Confidence == "reviewed" }

// TrustStoreID is the trust file's id for the store, the one entries and
// verdict files name.
func TrustStoreID(cfg config.Config, id route.StoreID) string {
	if id == route.StoreWork {
		return cfg.WorkStoreID
	}
	return cfg.PersonalStoreID
}

// VerdictPath is the verdict file of the store with trust-file id storeID.
func VerdictPath(storeID string) string {
	return filepath.Join(VerdictRoot, storeID, "verdicts")
}
