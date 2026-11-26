# sshd keys (development only)

`make keys` writes two files here, and `go:embed` builds them into the image:

- `host_ed25519`: godeep's host key (private), a copy of `bin/ssh/host_ed25519`
- `authorized_keys`: the public keys allowed to log in: `bin/ssh/id_ed25519.pub`,
  plus `SSH_AUTHORIZED_KEYS=path` if given

Both are ignored by git. Without `authorized_keys`, sshd refuses to start;
without a host key, it makes a new one at each boot.
