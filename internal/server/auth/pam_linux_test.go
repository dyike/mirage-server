package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestPAMPolicy(t *testing.T) {
	if _, err := PAM("system-auth"); err != nil {
		t.Fatal(err)
	}
	if pam.startConf == nil {
		t.Skip("pam_start_confdir unavailable")
	}
	dir := t.TempDir()
	for _, mode := range []string{"permit", "deny"} {
		if err := os.WriteFile(filepath.Join(dir, mode), []byte("auth required pam_"+mode+".so\naccount required pam_"+mode+".so\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := authenticate(context.Background(), mode, dir, "test-user", "test-password")
		if (err == nil) != (mode == "permit") {
			t.Fatalf("%s policy: %v", mode, err)
		}
	}
}
func TestPAMConversation(t *testing.T) {
	if _, err := PAM("system-auth"); err != nil {
		t.Fatal(err)
	}
	id := uintptr(pam.seq.Add(1))
	pam.requests.Store(id, credentials{"user", "secret"})
	defer pam.requests.Delete(id)
	messages := []*pamMessage{{Style: 2}, {Style: 1}, {Style: 4}}
	var response *pamResponse
	if rc := converse(3, &messages[0], &response, id); rc != 0 {
		t.Fatal(rc)
	}
	for i, r := range unsafe.Slice(response, 3) {
		if i < 2 && r.Text == nil {
			t.Fatal("missing response")
		}
		if r.Text != nil {
			pam.free(r.Text)
		}
	}
	pam.free(unsafe.Pointer(response))
	if rc := converse(33, &messages[0], &response, id); rc != 19 {
		t.Fatal("invalid conversation accepted")
	}
}
