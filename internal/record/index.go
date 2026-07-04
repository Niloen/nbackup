package record

import (
	"compress/gzip"
	"encoding/json"
	"io"
)

// index.go is the per-archive member index: the list of members an archive holds, stored
// as its own file (KindIndex) beside the archive's parts and read only for browse. It is
// kept out of the commit footer so a scan/rebuild reads only small footers; the member
// list — the large part of an archive's metadata — is paid for only when someone browses.

// indexDoc is the index's on-medium document. It is an object (not a bare array) so it
// can grow siblings of the member list — a framed archive's frame table ("frames") lands
// here without a format break.
type indexDoc struct {
	Members []Member `json:"members"`
}

// EncodeIndex writes an archive's member list as a gzip'd JSON document.
func EncodeIndex(w io.Writer, members []Member) error {
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(indexDoc{Members: members}); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// DecodeIndex reads a member list written by EncodeIndex.
func DecodeIndex(r io.Reader) ([]Member, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var doc indexDoc
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Members, nil
}
