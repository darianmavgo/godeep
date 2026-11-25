# godeep build
#
#   make qemu              arm64 kernel on QEMU virt, HVF-accelerated (default; Apple Silicon)
#   make qemu ACCEL=tcg    same, emulated (any host without HVF)
#   make qemu ARCH=amd64   x86-64 kernel on QEMU microvm (emulated on Apple Silicon)
#   make stop              from another terminal: power godeep off cleanly (falls back to kill)
#   make kill              from another terminal: kill QEMU immediately

# TamaGo toolchain: https://github.com/usbarmory/tamago-go (set TAMAGO to its bin/go)
TAMAGO ?= $(HOME)/tamago-go/bin/go
ARCH   ?= arm64

TAMAGO_ENV = GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=$(ARCH) GOEXPERIMENT=simd
# Serial console on stdio: ^C goes to the guest, Ctrl-A X quits QEMU.
SERIAL = -display none -chardev stdio,id=con,mux=on,signal=off -serial chardev:con -monitor none -no-reboot

ifeq ($(ARCH),arm64)
# board/virt. TEXT_START = RAM base (0x40000000) + 0x10000
TEXT_START  := 0x40010000
TAMAGO_TAGS ?=
QEMU        ?= qemu-system-aarch64
ACCEL       ?= hvf
ifeq ($(ACCEL),hvf)
CPU         ?= host
else
CPU         ?= max
endif
# -smp 1: TamaGo has no arm64 SMP bring-up yet, extra vCPUs would stay powered off.
QEMU_FLAGS  ?= -machine virt,gic-version=3 -accel $(ACCEL) -cpu $(CPU) -smp 1 -m 4G $(SERIAL)
else ifeq ($(ARCH),amd64)
# board/microvm, flags mirror usbarmory/tamago-example (TARGET=microvm).
# TEXT_START = amd64 ramStart + 0x10000. Needs -m 3G+ (RAM is mapped up to 0xb0000000).
TEXT_START  := 0x10010000
TAMAGO_TAGS ?= microvm
QEMU        ?= qemu-system-x86_64
# No KVM on macOS: TCG emulation with -cpu max.
QEMU_FLAGS  ?= -machine microvm,x-option-roms=on,pit=off,pic=off,rtc=on \
	-global virtio-mmio.force-legacy=false -cpu max -smp 1 -m 3G $(SERIAL)
else
$(error ARCH must be arm64 or amd64)
endif

TAMAGO_LDFLAGS ?= -s -w -T $(TEXT_START) -R 0x1000
KERNEL := bin/godeep-$(ARCH).elf
# Control files so `make stop` can find this VM and press its power button.
CONTROL = -pidfile bin/qemu-$(ARCH).pid -qmp unix:bin/qmp-$(ARCH).sock,server=on,wait=off

.PHONY: test hosted kernel qemu stop kill clean

test:
	go vet ./...
	go test ./...

hosted:
	go build -o bin/godeep-hosted ./cmd/godeep-hosted
	./bin/godeep-hosted

kernel:
	$(TAMAGO_ENV) $(TAMAGO) build -tags "$(TAMAGO_TAGS)" -trimpath -ldflags "$(TAMAGO_LDFLAGS)" -o $(KERNEL) ./cmd/godeep

qemu: kernel
	@echo "booting godeep $(ARCH)... (quit QEMU: Ctrl-A then X, or type halt)"
	@mkdir -p bin
	$(QEMU) $(QEMU_FLAGS) $(CONTROL) -kernel $(KERNEL)
	@rm -f bin/qemu-$(ARCH).pid bin/qmp-$(ARCH).sock

stop:
	@tools/qemu-stop.sh

kill:
	@tools/qemu-stop.sh --force

clean:
	rm -rf bin
