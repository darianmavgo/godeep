# TamaGo toolchain: https://github.com/usbarmory/tamago-go (set TAMAGO to its bin/go)
TAMAGO ?= $(HOME)/tamago-go/bin/go
# Flags mirror usbarmory/tamago-example (TARGET=microvm).
# TEXT_START = amd64 ramStart + 0x10000
TAMAGO_ENV := GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=amd64 GOEXPERIMENT=simd
TAMAGO_TAGS ?= microvm
TAMAGO_LDFLAGS ?= -s -w -T 0x10010000 -R 0x1000
QEMU ?= qemu-system-x86_64
# No KVM on macOS: QEMU uses TCG emulation (slower but fine).
QEMU_FLAGS ?= -machine microvm,x-option-roms=on,pit=off,pic=off,rtc=on \
	-global virtio-mmio.force-legacy=false -cpu max -smp 1 -m 3G \
	-display none -chardev stdio,id=con,mux=on,signal=off -serial chardev:con -monitor none -no-reboot

.PHONY: test hosted kernel qemu clean

test:
	go vet ./...
	go test ./...

hosted:
	go build -o bin/godeep-hosted ./cmd/godeep-hosted
	./bin/godeep-hosted

kernel:
	$(TAMAGO_ENV) $(TAMAGO) build -tags $(TAMAGO_TAGS) -trimpath -ldflags "$(TAMAGO_LDFLAGS)" -o bin/godeep.elf ./cmd/godeep

qemu: kernel
	@echo "booting godeep... (quit QEMU: Ctrl-A then X)"
	$(QEMU) $(QEMU_FLAGS) -kernel bin/godeep.elf

clean:
	rm -rf bin
