package record

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestIndexRoundTrip pins the per-archive member index encode/decode cycle: a member
// list written by EncodeIndex reads back identically through DecodeIndex.
func TestIndexRoundTrip(t *testing.T) {
	members := []string{"./", "./etc/", "./etc/hosts", "./var/log/a.log"}
	var buf bytes.Buffer
	if err := EncodeIndex(&buf, members); err != nil {
		t.Fatalf("EncodeIndex: %v", err)
	}
	got, err := DecodeIndex(&buf)
	if err != nil {
		t.Fatalf("DecodeIndex: %v", err)
	}
	if !reflect.DeepEqual(got, members) {
		t.Errorf("round-trip = %v, want %v", got, members)
	}
}

// TestDecodeIndexMalformed confirms DecodeIndex rejects non-gzip input rather than
// returning garbage.
func TestDecodeIndexMalformed(t *testing.T) {
	if _, err := DecodeIndex(strings.NewReader("not gzip data")); err == nil {
		t.Error("DecodeIndex of non-gzip input should error")
	}
}
