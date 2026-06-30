package media

import (
	"strings"
	"testing"
)

func TestValidateParams(t *testing.T) {
	Register(Spec{Type: "widget", Params: []string{"path", "size"}})

	// A known key is accepted; an unknown one is rejected, naming it and the allowed set.
	if err := ValidateParams("widget", map[string]string{"path": "/x"}); err != nil {
		t.Errorf("known key rejected: %v", err)
	}
	err := ValidateParams("widget", map[string]string{"pth": "/x"})
	if err == nil {
		t.Fatal("typo'd key accepted, want error")
	}
	if !strings.Contains(err.Error(), "pth") || !strings.Contains(err.Error(), "path") {
		t.Errorf("error should name the unknown key and the accepted ones: %v", err)
	}

	// An unregistered type is not validated (lenient), so unknown media types don't
	// spuriously fail before OpenVolume reports them.
	if err := ValidateParams("unregistered", map[string]string{"anything": "1"}); err != nil {
		t.Errorf("unregistered type should be lenient, got: %v", err)
	}
}
