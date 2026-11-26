// ninepd serves a directory over 9P2000.L on TCP, for godeep to mount
// (mount9p HOST:PORT NAME): the dev machine's files on the Mac, or in QEMU
// through its NAT (the host is 10.0.2.2 from the guest).
//
//	go run ./tools/ninepd -addr 127.0.0.1:5640 -dir bin/share [-ro]
//
// Every path is resolved inside -dir with os.Root, so a client can't
// escape it (no "..", no symlinks out). It speaks what fs/ninep uses:
// version, attach, walk, getattr, lopen, lcreate, read, write, readdir,
// setattr (size), mkdir, unlinkat, renameat, clunk.
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"

	"godeep/fs/ninep"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5640", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	ro := flag.Bool("ro", false, "read-only")
	flag.Parse()
	root, err := os.OpenRoot(*dir)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("ninepd: serving %s on %s (read-only: %v)", *dir, l.Addr(), *ro)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := Serve(c, root, *ro); err != nil && !errors.Is(err, io.EOF) {
				log.Printf("ninepd: %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

// Serve answers one connection's requests in order.
func Serve(c io.ReadWriteCloser, root *os.Root, ro bool) error {
	return ninep.Serve(c, root, ro)
}
