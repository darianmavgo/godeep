# godeep build
#
#   make qemu              arm64 kernel on QEMU virt, HVF-accelerated (default; Apple Silicon)
#   make qemu ACCEL=tcg    same, emulated (any host without HVF)
#   make qemu ARCH=amd64   x86-64 kernel on QEMU microvm (emulated on Apple Silicon)
#   make qemu EL2=1        arm64, entered at EL2 like m1n1 does on a real Mac (TCG only)
#   make qemu BOOT=elf     arm64 ELF instead of the Linux-style Image (no device tree)
#   make image             arm64 Image (bin/godeep-arm64.img) for QEMU, m1n1, U-Boot booti
#   make image BOARD=applem1   Image for this MacBook Pro (bin/godeep-applem1.img), see docs/TargetMyMac.md
#   make qemu-applem1      that same Mac image on QEMU sbsa-ref (RAM at 1 TB) via tools/fakem1n1,
#                          handed over like m1n1 does (EL2, E2H=1, relocation, m1n1-style tree)
#   make shot-applem1      (another terminal) save its framebuffer as bin/fakem1n1/screen.png
#   make native            test, rehearse on QEMU, install on this Mac (DRY=1: dry run)
#   make check-virt        QEMU virt headless: PCIe + NVMe + FAT/ext4 from the shell
#   make check-applem1     boot it headless and check the shell comes up, then check the
#                          fault screen at stages 1 and 3 (about a minute; TCG)
#   make keys              SSH keys for sshd (bin/ssh; built into the image)
#   make ssh               ssh into godeep (QEMU virt or hosted: localhost:$(SSH_PORT))
#   make hot               like make qemu, but rebuilds and reboots godeep whenever a source file
#                          changes (Ctrl-A x quits, Ctrl-A r reboots now); see tools/hot
#   make wasm              build examples/wasm/* for WASI (bin/wasm/NAME.wasm)
#   make push WASM=hello   copy bin/wasm/hello.wasm to godeep's ram: over SSH; run it: ram:/hello.wasm
#   make stop              from another terminal: power godeep off cleanly (falls back to kill)
#   make kill              from another terminal: kill QEMU immediately

# TamaGo toolchain: https://github.com/usbarmory/tamago-go (set TAMAGO to its bin/go)
TAMAGO ?= $(HOME)/tamago-go/bin/go
ARCH   ?= arm64

TAMAGO_ENV = GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=$(ARCH) GOEXPERIMENT=simd
# Serial console on stdio: ^C goes to the guest, Ctrl-A X quits QEMU.
# QEMU_DISPLAY=cocoa opens a window on the ramfb framebuffer console (the
# USB mouse drives it: click in the window to grab the mouse, Ctrl-Alt-G
# to let go).
QEMU_DISPLAY ?= none
SERIAL = -display $(QEMU_DISPLAY) -chardev stdio,id=con,mux=on,signal=off -serial chardev:con -monitor none -no-reboot

ifeq ($(ARCH),arm64)
BOARD       ?= virt
# linkcpuinit: the board supplies the entry code (relocation, EL2 drop,
#   device tree capture, early identity map)
# linkhwinit0: and the MMU setup (kernel/pgtable: 4-level, 48-bit)
ifeq ($(BOARD),applem1)
# board/applem1: RAM at DRAM base (1 TB) + 4 GB, kernel 1 MB in (mem.go,
# entry_arm64.s IMAGE_BASE = TEXT_START - 0x1000)
TEXT_START  := 0x10100100000
RAM_BASE    := 0x10100000000
TAMAGO_TAGS ?= linkcpuinit,linkhwinit0,applem1
else
# board/virt. TEXT_START = RAM base (0x40000000) + 0x10000
TEXT_START  := 0x40010000
RAM_BASE    := 0x40000000
TAMAGO_TAGS ?= linkcpuinit,linkhwinit0
endif
QEMU        ?= qemu-system-aarch64
# BOOT=image (default): Linux-style Image, QEMU passes the device tree in x0
# like m1n1 and U-Boot do. BOOT=elf: the ELF, no device tree.
BOOT        ?= image
EL2         ?=
ifeq ($(EL2),1)
ACCEL       := tcg
MACHINE_EL2 := ,virtualization=on
endif
ACCEL       ?= hvf
ifeq ($(ACCEL),hvf)
CPU         ?= host
else
CPU         ?= max
endif
# -smp 1: TamaGo has no arm64 SMP bring-up yet, extra vCPUs would stay powered off.
QEMU_FLAGS  ?= -machine virt,gic-version=3$(MACHINE_EL2) -accel $(ACCEL) -cpu $(CPU) -smp 1 -m 4G $(SERIAL)
# An NVMe disk (read-only) with a FAT32 and an ext4 partition: lsblk, ls, cat.
DISK        ?= bin/disk.img
# The NVMe disk is a writable copy (bin/disk-rw.img: FAT writes persist
# across make qemu / make hot reboots; check-virt starts from a fresh copy).
DISK_RW     ?= bin/disk-rw.img
QEMU_DISK   := -drive if=none,id=disk0,file=$(DISK_RW),format=raw -device nvme,serial=godeep0,drive=disk0
# USB: an xHCI controller with a keyboard and a mouse (headless, they get
# input from QMP input-send-event; see check-virt).
QEMU_DISK   += -device qemu-xhci,id=xhci -device usb-kbd,bus=xhci.0 -device usb-mouse,bus=xhci.0
# ...and a USB hub with a USB stick behind it (the same test image, read-only):
# hub enumeration, routing through it, bulk transfers, BOT/SCSI.
QEMU_DISK   += -device usb-hub,bus=xhci.0,port=3 \
	-drive if=none,id=usbstick,file=$(DISK),format=raw,readonly=on -device usb-storage,bus=xhci.0,port=3.1,drive=usbstick
# The repository is shared read-only as godeep's "host" volume (virtio-9p):
# host:/bin/wasm/hello.wasm runs a program right after `make wasm`.
# Network and entropy: virtio-net on QEMU's user network (guest 10.0.2.15,
# host port SSH_PORT forwarded to the guest's sshd), and virtio-rng. Modern
# virtio-mmio (version 2) only: godeep has no legacy transport.
SSH_PORT    ?= 2222
QEMU_NET    = -device ramfb -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	-netdev user,id=net0,hostfwd=tcp:127.0.0.1:$(SSH_PORT)-:22 -device virtio-net-device,netdev=net0 \
	-fsdev local,id=share0,path=$(CURDIR),security_model=none,readonly=on -device virtio-9p-device,fsdev=share0,mount_tag=host
QEMU_DEPS   := $(DISK) $(DISK_RW)
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
ifeq ($(BOARD),applem1)
KERNEL := bin/godeep-applem1.elf
IMAGE  := bin/godeep-applem1.img
else
KERNEL := bin/godeep-$(ARCH).elf
IMAGE  := bin/godeep-$(ARCH).img
endif
ifeq ($(ARCH)-$(BOOT),arm64-image)
BOOTFILE := $(IMAGE)
else
BOOTFILE := $(KERNEL)
endif
# Control files so `make stop` can find this VM and press its power button.
CONTROL = -pidfile bin/qemu-$(ARCH).pid -qmp unix:bin/qmp-$(ARCH).sock,server=on,wait=off

.PHONY: wasm push hot check-usbnet test hosted kernel image qemu check-virt qemu-applem1 shot-applem1 check-applem1 native stop kill clean keys ssh

# ---- SSH keys (development) ----
# bin/ssh keeps them across builds: host_ed25519 is godeep's host key,
# id_ed25519 the client key `make ssh` logs in with. make keys copies the
# host key and the authorized public keys into programs/sshd/keys/
# (gitignored), where go:embed builds them into the image.
# SSH_AUTHORIZED_KEYS=path adds your own key(s) too.
SSH_DIR  := bin/ssh
SSH_OPTS  = -i $(SSH_DIR)/id_ed25519 -p $(SSH_PORT) -o UserKnownHostsFile=$(SSH_DIR)/known_hosts \
	-o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes -o ConnectTimeout=5
SCP_OPTS  = -q -P $(SSH_PORT) -i $(SSH_DIR)/id_ed25519 -o UserKnownHostsFile=$(SSH_DIR)/known_hosts \
	-o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes
SSH_AUTHORIZED_KEYS ?=

$(SSH_DIR)/host_ed25519 $(SSH_DIR)/id_ed25519:
	@mkdir -p $(SSH_DIR)
	ssh-keygen -q -t ed25519 -N '' -C godeep-$(notdir $@) -f $@

keys: $(SSH_DIR)/host_ed25519 $(SSH_DIR)/id_ed25519
	@cp $(SSH_DIR)/host_ed25519 programs/sshd/keys/host_ed25519
	@cat $(SSH_DIR)/id_ed25519.pub $(SSH_AUTHORIZED_KEYS) > programs/sshd/keys/authorized_keys.tmp
	@cmp -s programs/sshd/keys/authorized_keys.tmp programs/sshd/keys/authorized_keys 2>/dev/null || \
		mv programs/sshd/keys/authorized_keys.tmp programs/sshd/keys/authorized_keys
	@rm -f programs/sshd/keys/authorized_keys.tmp

# WASM programs (programs/wasm): built for WASI with the host Go, copied to
# godeep's RAM filesystem with scp (SFTP), run from the shell with no reboot.
WASM_PROGS := $(notdir $(wildcard examples/wasm/*))
WASM ?= hello
wasm:
	@mkdir -p bin/wasm
	@for p in $(WASM_PROGS); do GOOS=wasip1 GOARCH=wasm go build -o bin/wasm/$$p.wasm ./examples/wasm/$$p && echo "bin/wasm/$$p.wasm"; done

push: wasm
	scp $(SCP_OPTS) bin/wasm/$(WASM).wasm root@127.0.0.1:
	@echo "on godeep: ram:/$(WASM).wasm"

# ssh into godeep: `make ssh`, or `make ssh CMD='lsdev net'`.
ssh: $(SSH_DIR)/id_ed25519
	ssh $(SSH_OPTS) root@127.0.0.1 $(CMD)

# -stdmethods=false: wazero's experimental sys.File needs Seek(int64, int)
# (int64, Errno), not io.Seeker's signature (programs/wasm/sysfs.go).
test:
	go vet -stdmethods=false ./...
	go test ./...

hosted: keys
	go build -o bin/godeep-hosted ./cmd/godeep-hosted
	./bin/godeep-hosted

kernel: keys
	$(TAMAGO_ENV) $(TAMAGO) build -tags "$(TAMAGO_TAGS)" -trimpath -ldflags "$(TAMAGO_LDFLAGS)" -o $(KERNEL) ./cmd/godeep

image: kernel
	go run ./tools/mkimage -ram $(RAM_BASE) -o $(IMAGE) $(KERNEL)

qemu: $(if $(filter $(BOOTFILE),$(IMAGE)),image,kernel) $(QEMU_DEPS)
	@if [ "$(BOARD)" = applem1 ]; then echo "BOARD=applem1 runs on QEMU sbsa-ref, not virt: make qemu-applem1"; exit 1; fi
	@echo "booting godeep $(ARCH)... (quit QEMU: Ctrl-A then X, or type halt)"
	@mkdir -p bin
	$(QEMU) $(QEMU_FLAGS) $(QEMU_DISK) $(QEMU_NET) $(CONTROL) -kernel $(BOOTFILE)
	@rm -f bin/qemu-$(ARCH).pid bin/qmp-$(ARCH).sock

# make hot: make qemu that reboots into a fresh build on every source change.
# tools/hot owns the terminal and QEMU's serial port (a socket, so the
# terminal survives reboots), rebuilds with the same ARCH/BOARD settings,
# keeps the old VM running if the build fails, and powers godeep off
# cleanly over QMP before booting the new image. ssh keeps working: the host
# key is stable across builds.
hot: SERIAL = -display $(QEMU_DISPLAY) -chardev socket,id=con,path=bin/hot-con.sock,server=on,wait=on -serial chardev:con -monitor none -no-reboot
hot: keys $(QEMU_DEPS)
	@if [ "$(BOARD)" = applem1 ]; then echo "make hot runs QEMU virt (BOARD=applem1 runs on sbsa-ref: make qemu-applem1)"; exit 1; fi
	@go build -o bin/hot ./tools/hot
	@bin/hot -build '$(MAKE) -s $(if $(filter $(BOOTFILE),$(IMAGE)),image,kernel)' -serial bin/hot-con.sock -qmp bin/qmp-$(ARCH).sock \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_DISK) $(QEMU_NET) $(CONTROL) -kernel $(BOOTFILE)
	@rm -f bin/qemu-$(ARCH).pid bin/qmp-$(ARCH).sock bin/hot-con.sock

# QEMU virt, headless: boot, read the NVMe disk's partitions through PCIe,
# NVMe, GPT, FAT32 and ext4 from the shell, then type on the USB keyboard
# and move the USB mouse through QMP (tools/qemucheck); then the network:
# virtio-rng seeds crypto/rand, virtio-net + the TCP/IP stack come up, and
# the host logs in over SSH (its own port, so a running `make qemu` is fine).
# The USB mouse drives the ramfb console: the pointer goes to the prompt,
# drags over "godeep", and a right click pastes it into the shell. The
# screen ends up in bin/check-virt-screen.png.
check-virt: SSH_PORT = 2223
check-virt: DISK_RW = bin/disk-check.img
check-virt: wasm
	@if [ "$(ARCH)" != arm64 ] || [ "$(BOARD)" = applem1 ]; then echo "check-virt is for ARCH=arm64 on QEMU virt"; exit 1; fi
	@$(MAKE) -s image $(DISK)
	@cp $(DISK) $(DISK_RW)
	@rm -rf bin/ninep-check && mkdir -p bin/ninep-check && printf 'hello over tcp 9p\n' > bin/ninep-check/seed.txt
	go run ./tools/qemucheck \
	  -step 'lsdev pci nvme=>nvme0,QEMU NVMe Ctrl,ready' \
	  -step 'lsblk=>nvme0p1,FAT32,GODEEPFAT,nvme0p2,ext4,godeep-ext4' \
	  -step 'ls nvme0p2:/=>docs/,link-to-hello@,big.bin' \
	  -step 'cat nvme0p2:/link-to-hello=>hello from godeep' \
	  -step 'cat nvme0p1:/many/f123.txt=>file 123' \
	  -step 'ls nvme0p1:/docs=>readme.md' \
	  -step 'lsdev xhci=>HID boot keyboard,HID boot mouse' \
	  -step 'lsdev xhci=>hub,mass storage' -step 'lsblk=>usbdisk0p1,FAT32,usbdisk0p2,ext4' \
	  -step 'cat usbdisk0p2:/hello.txt=>hello from godeep' \
	  -step 'keys:help\r' -step 'wait:BUILTIN' \
	  -step 'send:evtest 4' -step 'wait:listening' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"rel","data":{"axis":"x","value":10}},{"type":"rel","data":{"axis":"y","value":-5}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":true,"button":"left"}}]}}' \
	  -step 'wait:motion dx=10 dy=-5' -step 'wait:button 0 down' \
	  -step 'lsdev xhci=>keys typed' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":false,"button":"left"}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"rel","data":{"axis":"x","value":-3000}},{"type":"rel","data":{"axis":"y","value":3000}}]}}' \
	  -step 'lsdev fbcon=>1280x800 px,pointer at 0,799' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":true,"button":"left"}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"rel","data":{"axis":"x","value":40}},{"type":"rel","data":{"axis":"y","value":0}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":false,"button":"left"}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":true,"button":"right"}}]}}' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":false,"button":"right"}}]}}' \
	  -step 'wait:godeep' -step 'send:' -step 'wait:godeep: command not found' \
	  -step 'qmp:{"execute":"screendump","arguments":{"filename":"bin/check-virt-screen.png","format":"png"}}' \
	  -step 'lsdev rng0=>seeded crypto/rand' \
	  -step 'ifconfig=>10.0.2.15/24,52:54:00,dhcp from 10.0.2.2,dns 10.0.2.3' \
	  -step 'get http://10.0.2.2:2280/go.mod=>module godeep' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 ps=>sshd,netd,PID' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 lsdev net0=>virtio,device1,ready,rx' \
	  -step 'local:echo uptime | ssh $(SSH_OPTS) -tt root@127.0.0.1=>godeep$$ ' \
	  -step 'lsdev net0=>tx' \
	  -step 'local:scp $(SCP_OPTS) bin/wasm/hello.wasm root@127.0.0.1:=>' \
	  -step 'df=>ram,memfs,rw,nvme0p2,ext4,ro' \
	  -step 'ls ram:/=>hello.wasm' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 ram:/hello.wasm a b=>hello from WebAssembly,["a" "b"],/ram,/nvme0p2/hello.txt: hello from godeep' \
	  -step 'ram:/hello.wasm from the console=>args: ["from" "the" "console"]' \
	  -step 'df=>host,9p' -step 'cat host:/go.mod=>module godeep' \
	  -step 'host:/bin/wasm/hello.wasm straight from the repo=>hello from WebAssembly,"straight",/host' \
	  -step 'mount9p 10.0.2.2:5641 dev=>mounted 10.0.2.2:5641 as dev: (rw)' \
	  -step 'df=>dev,9p,rw' \
	  -step 'cat dev:/seed.txt=>hello over tcp 9p' \
	  -step 'GREETING=hi WRITE_FILE=/dev/sub/from-wasm.txt host:/bin/wasm/hello.wasm written over 9p=>env: GREETING=hi,wrote /dev/sub/from-wasm.txt' \
	  -step 'cat dev:/sub/from-wasm.txt=>written over 9p' \
	  -step 'local:cat bin/ninep-check/sub/from-wasm.txt=>written over 9p' \
	  -step 'umount dev=>' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 dmesg=>sshd: listening,logged in' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 pprof heap > bin/check-heap.pb.gz && go tool pprof -top bin/check-heap.pb.gz=>flat%' \
	  -qmp bin/qmp-check.sock -http 127.0.0.1:2280=. -ninep 127.0.0.1:5641=bin/ninep-check \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_DISK) $(QEMU_NET) -qmp unix:bin/qmp-check.sock,server=on,wait=off -kernel $(BOOTFILE)
	@$(MAKE) -s check-usbnet

# USB Ethernet (CDC ECM) as the only network: QEMU's usb-net on the xHCI,
# DHCP and SSH over it. The Mac's USB-C gadget link will look like this
# from godeep's side. Run by check-virt.
check-usbnet:
	@$(MAKE) -s image
	go run ./tools/qemucheck -log bin/qemucheck-usbnet.log \
	  -step 'lsdev usbnet=>usbnet0,godeep,usb-ecm,ready' -step 'ifconfig=>dhcp from 10.0.2.2' \
	  -step 'local:ssh -i $(SSH_DIR)/id_ed25519 -p 2224 -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@127.0.0.1 lsdev usbnet0=>tx' \
	  -- $(QEMU) $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -device qemu-xhci,id=xhci -netdev user,id=u0,hostfwd=tcp:127.0.0.1:2224-:22 -device usb-net,netdev=u0,bus=xhci.0 \
	  -kernel $(BOOTFILE)

$(DISK_RW): $(DISK)
	cp $(DISK) $@

bin/disk.img: tools/mkdisk/main.go kernel/block/gptwrite.go $(wildcard fs/testdata/*.img.gz)
	go run ./tools/mkdisk -o $@

# The Mac image under QEMU (tools/fakem1n1). Always BOARD=applem1, whatever
# was given; sbsa-ref needs EL3, so TCG only.
APPLEM1 = $(MAKE) -s image BOARD=applem1

qemu-applem1:
	@$(APPLEM1)
	@go run ./tools/fakem1n1 run

shot-applem1:
	@go run ./tools/fakem1n1 shot

check-applem1:
	@$(APPLEM1)
	@go build -o bin/fakem1n1-check ./tools/fakem1n1
	bin/fakem1n1-check check
	bin/fakem1n1-check check -fault 1 -o bin/fakem1n1/fault1
	bin/fakem1n1-check check -fault 3 -o bin/fakem1n1/fault3

# The Mac itself: host tests, build bin/godeep-applem1.img, rehearse it on
# QEMU (check-applem1), then install it on the godeep boot partition so m1n1
# boots it directly. The install asks you to type "godeep" and for your
# sudo password, so run it in a terminal. DRY=1 shows the install steps
# without changing anything.
native:
	@$(MAKE) -s test
	@$(MAKE) -s image BOARD=applem1
	@echo "built bin/godeep-applem1.img"
	@$(MAKE) -s check-applem1
	tools/godeep-partition.sh install $(if $(DRY),--dry-run)

stop:
	@tools/qemu-stop.sh

kill:
	@tools/qemu-stop.sh --force

clean:
	rm -rf bin
