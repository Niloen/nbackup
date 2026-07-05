package record

import (
	"compress/gzip"
	"encoding/json"
	"io"
)

// index.go is the per-archive index: the members an archive holds plus, for a framed
// archive, its frame table — stored as one file (KindIndex) beside the archive's parts
// and read only for browse and ranged reads. It is kept out of the commit footer so a
// scan/rebuild reads only small footers; the member list — the large part of an
// archive's metadata — is paid for only when someone browses.

// Index is the per-archive index document: the member list, for a framed archive
// the decode-restart table, and the archiver's content inventory (see Unit). Absent
// frames mean a plain stream — exactly today's read path; the table is an
// optimization for ranged reads, never decode-critical. Absent units mean the
// archiver reports no inventory (gnutar, pipe) — `--inventory` then says so.
type Index struct {
	Members []Member `json:"members"`
	Frames  []Frame  `json:"frames,omitempty"`
	Units   []Unit   `json:"units,omitempty"`
}

// EncodeIndex writes an archive's index as a gzip'd JSON document.
func EncodeIndex(w io.Writer, idx Index) error {
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(idx); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// DecodeIndex reads an index written by EncodeIndex.
func DecodeIndex(r io.Reader) (Index, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Index{}, err
	}
	defer gz.Close()
	var doc Index
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		return Index{}, err
	}
	return doc, nil
}
