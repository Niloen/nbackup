package record

import (
	"compress/gzip"
	"encoding/json"
	"io"
)

// index.go is the per-archive member index: the list of paths an archive holds, stored as
// its own file (KindIndex) beside the archive's parts and read only for browse. It is kept
// out of the commit footer so a scan/rebuild reads only small footers; the member list — the
// large part of an archive's metadata — is paid for only when someone browses.

// EncodeIndex writes an archive's member list as a gzip'd JSON array.
func EncodeIndex(w io.Writer, members []string) error {
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(members); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// DecodeIndex reads a member list written by EncodeIndex.
func DecodeIndex(r io.Reader) ([]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var members []string
	if err := json.NewDecoder(gz).Decode(&members); err != nil {
		return nil, err
	}
	return members, nil
}
