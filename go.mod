module godeep

go 1.27.3

require (
	github.com/google/pprof v0.0.0-20261118010117-084b33a85f63
	github.com/pkg/sftp v1.13.13
	github.com/tetratelabs/wazero v1.12.0
	github.com/usbarmory/tamago v1.27.4
	golang.org/x/crypto v0.59.0
	golang.org/x/image v0.48.0
	golang.org/x/net v0.63.0
	gvisor.dev/gvisor v0.0.0-20261126070802-b1c44a434dad
)

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/kr/fs v0.1.0 // indirect
	golang.org/x/crypto/x509roots/fallback v0.0.0-20261123192653-4dc4fb521066 // indirect
	golang.org/x/exp v0.0.0-20250711185948-6ae5c78190dc // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/term v0.48.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/usbarmory/tamago => ./third_party/tamago
