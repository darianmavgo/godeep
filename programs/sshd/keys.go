package sshd

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/crypto/ssh"
)

// keys/ holds the development keys `make keys` writes (gitignored; only its
// README is tracked).
//
//go:embed keys
var keyFS embed.FS

// BuiltinKeys returns the host key and authorized keys built into the
// image. A missing host key is generated (a new one each boot); missing
// authorized keys are an empty list.
func BuiltinKeys() (ssh.Signer, []ssh.PublicKey, error) {
	var host ssh.Signer
	if pem, err := keyFS.ReadFile("keys/host_ed25519"); err == nil {
		if host, err = ssh.ParsePrivateKey(pem); err != nil {
			return nil, nil, fmt.Errorf("keys/host_ed25519: %w", err)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if host, err = NewHostKey(); err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, err
	}
	auth, err := keyFS.ReadFile("keys/authorized_keys")
	if errors.Is(err, fs.ErrNotExist) {
		return host, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	keys, err := ParseAuthorizedKeys(auth)
	return host, keys, err
}

// ParseAuthorizedKeys reads an OpenSSH authorized_keys file.
func ParseAuthorizedKeys(b []byte) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("authorized_keys line %d: %w", n, err)
		}
		keys = append(keys, k)
	}
	return keys, sc.Err()
}
