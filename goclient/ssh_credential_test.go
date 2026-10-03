// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A real-shaped Protect value: "$6$" + 16-char salt + 86-char digest.
const testProtectHash = "$6$xZzCrHxvLAitFAF4$EPVaxqBrbmIJMDKEuJSmeqhmn3loB6CHm9qhLWD6Q.slRN8LUxGupI5b7iLGk9iDhf1EBQhmzD/BbrnxLBlSL."

func TestValidSSHCredential(t *testing.T) {
	ok := []struct{ user, hash string }{
		{"ubnt", testProtectHash},
		{"ui_admin-2", testProtectHash},
		{"ubnt", "$6$rounds=5000$abc$" + testProtectHash[20:]},
	}
	for _, c := range ok {
		if err := validSSHCredential(c.user, c.hash); err != nil {
			t.Errorf("%q/%q rejected: %v", c.user, c.hash, err)
		}
	}
	bad := []struct{ user, hash string }{
		{"", testProtectHash},
		{"Ubnt", testProtectHash},
		{"1ubnt", testProtectHash},
		{"ubnt:x:0:0", testProtectHash},
		{"ub/nt", testProtectHash},
		{"ubnt\nroot", testProtectHash},
		{"ubnt", ""},
		{"ubnt", "$1$abcdefgh$0123456789012345678901"},
		{"ubnt", testProtectHash + "\nroot::0:0"},
		{"ubnt", testProtectHash + ":"},
		{"ubnt", "plaintext-password"},
	}
	for _, c := range bad {
		if err := validSSHCredential(c.user, c.hash); err == nil {
			t.Errorf("%q/%q accepted", c.user, c.hash)
		}
	}
}

func TestStoreSSHCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_protect")
	changed, err := storeSSHCredential(path, "ubnt", testProtectHash)
	if err != nil || !changed {
		t.Fatalf("first store: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "ubnt:"+testProtectHash+"\n" {
		t.Fatalf("file = %q", got)
	}
	if changed, err = storeSSHCredential(path, "ubnt", testProtectHash); err != nil || changed {
		t.Fatalf("identical store: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// PROTECT_SSH off (the default): a valid push is acked by the caller but
// nothing is stored, so the credential can never reach dropbear.
func TestUpdateUsernamePasswordOffStoresNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_protect")
	oldPath, oldOn := sshProtectPath, cfg.ProtectSSH
	sshProtectPath, cfg.ProtectSSH = path, false
	defer func() { sshProtectPath, cfg.ProtectSSH = oldPath, oldOn }()

	c := &Client{}
	c.handleUpdateUsernamePassword(Envelope{Payload: map[string]interface{}{
		"username": "ui", "hashedPassword": testProtectHash,
	}})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential stored with PROTECT_SSH off: %v", err)
	}
}
