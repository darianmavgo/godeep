//go:build tamago && arm64 && !applem1

package virt

import (
	"crypto/aes"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	_ "unsafe"
)

// The Go runtime needs a random source (map seeds, crypto/rand). QEMU virt
// has no simple TRNG register, so this is an AES-CTR fast-key-erasure DRBG,
// following tamago/internal/rng (which can't be imported from outside
// TamaGo).
//
// The first seed is the boot-time counter value, which is NOT secure
// entropy. When QEMU has a virtio-rng device (-device virtio-rng-device,
// the Makefile adds one), its driver calls mixSeed with 64 bytes of the
// host's entropy as soon as it starts; lsdev rng0 says "seeded crypto/rand".
// Anything random made before that (map seeds, early keys) is predictable.
var drbg struct {
	sync.Mutex
	seed [32]byte
}

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	c := CPU.Counter()
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(drbg.seed[i*8:], c*0x9e3779b97f4a7c15+uint64(i))
	}
}

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	var counter uint64
	var block [aes.BlockSize]byte
	inc := func() {
		counter++
		binary.LittleEndian.PutUint64(block[:], counter)
	}

	drbg.Lock()
	c, err := aes.NewCipher(drbg.seed[:])
	if err != nil {
		panic(err)
	}
	// key erasure: derive the next seed before producing output
	c.Encrypt(drbg.seed[:aes.BlockSize], block[:])
	inc()
	c.Encrypt(drbg.seed[aes.BlockSize:], block[:])
	inc()
	drbg.Unlock()

	for len(b) >= aes.BlockSize {
		c.Encrypt(b[:aes.BlockSize], block[:])
		inc()
		b = b[aes.BlockSize:]
	}
	if len(b) > 0 {
		c.Encrypt(block[:], block[:])
		copy(b, block[:])
	}
}

// mixSeed folds entropy into the generator's key: the new key is
// SHA-256(old key || entropy), so it is at least as unpredictable as either.
func mixSeed(entropy []byte) {
	drbg.Lock()
	defer drbg.Unlock()
	h := sha256.New()
	h.Write(drbg.seed[:])
	h.Write(entropy)
	copy(drbg.seed[:], h.Sum(nil))
}
