// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

package main

// UpdateUsernamePassword {username, hashedPassword: $6$ crypt}, sent on every connect.
// With PROTECT_SSH, "user:hash" -> etc/ssh_protect for ssh-accounts.sh (README: Access).

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
)

var (
	sshProtectPath  = unifiPrefix + "/etc/ssh_protect"
	sshAccountsPath = unifiPrefix + "/script/ssh-accounts.sh"

	// Lands in /etc/passwd: same rule as ssh-accounts.sh, plus a length cap.
	sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// SHA-512 crypt: optional rounds=, salt of up to 16, 86-char digest.
	sshHashRe = regexp.MustCompile(`^\$6\$(rounds=[0-9]{1,9}\$)?[./0-9A-Za-z]{1,16}\$[./0-9A-Za-z]{86}$`)
)

// validSSHCredential checks the controller's payload before any of it reaches
// a file that sshd or /etc/passwd parses.
func validSSHCredential(user, hash string) error {
	if !sshUserRe.MatchString(user) {
		return fmt.Errorf("username %q not accepted", user)
	}
	if !sshHashRe.MatchString(hash) {
		return fmt.Errorf("hashedPassword is not a SHA-512 crypt string")
	}
	return nil
}

// storeSSHCredential writes "user:hash" to path (temp + rename). Reports
// whether the content changed.
func storeSSHCredential(path, user, hash string) (bool, error) {
	line := user + ":" + hash + "\n"
	if old, err := os.ReadFile(path); err == nil && string(old) == line {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) handleUpdateUsernamePassword(m Envelope) {
	if !cfg.ProtectSSH {
		return // PROTECT_SSH off: ack only, store nothing
	}
	user, _ := m.Payload["username"].(string)
	hash, _ := m.Payload["hashedPassword"].(string)
	if err := validSSHCredential(user, hash); err != nil {
		log.Printf("UpdateUsernamePassword: ignored: %v", err)
		return
	}
	changed, err := storeSSHCredential(sshProtectPath, user, hash)
	if err != nil {
		log.Printf("UpdateUsernamePassword: storing credential for %q: %v", user, err)
		return
	}
	if !changed {
		return
	}
	cmd := exec.Command("/bin/sh", sshAccountsPath)
	cmd.Env = append(os.Environ(), "UNIFI_PREFIX="+unifiPrefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("UpdateUsernamePassword: ssh-accounts.sh: %v: %s", err, out)
		return
	}
	log.Printf("UpdateUsernamePassword: device credential for %q installed", user)
}
