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

# TamaGo toolchain: https://github.com/usbarmory/tamago-go (set TAMAGO to its bin/go).
# make tamago-go builds bin/tamago-go: a copy of ~/tamago-go with godeep's
# runtime patch (third_party/tamago-go/godeep-runtime.patch: SMP wake-ups);
# it's used once it exists, else ~/tamago-go as is.
TAMAGO_SRC ?= $(HOME)/tamago-go
TAMAGO ?= $(if $(wildcard $(CURDIR)/bin/tamago-go/bin/go),$(CURDIR)/bin/tamago-go/bin/go,$(TAMAGO_SRC)/bin/go)
# godeepruntime: the toolchain has the patch's CPU-fault panics
# (runtime·tamagoFault), so third_party/tamago's vectors can use them.
GODEEP_RUNTIME_TAG = $(if $(wildcard $(dir $(TAMAGO))../src/runtime/sigpanic_tamago_arm64.s),$(comma)godeepruntime)
comma := ,
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
# SMP=N: N vCPUs; board/virt starts them with PSCI CPU_ON and sets GOMAXPROCS=N
# (board/virt/smp.go). 1 by default.
SMP         ?= 1
QEMU_FLAGS  ?= -machine virt,gic-version=3$(MACHINE_EL2) -accel $(ACCEL) -cpu $(CPU) -smp $(SMP) -m 4G $(SERIAL)
# An NVMe disk (read-only) with a FAT32 and an ext4 partition: lsblk, ls, cat.
DISK        ?= bin/disk.img
# The NVMe disk is a writable copy (bin/disk-rw.img: FAT writes persist
# across make qemu / make hot reboots; check-virt starts from a fresh copy).
DISK_RW     ?= bin/disk-rw.img
QEMU_DISK   := -drive if=none,id=disk0,file=$(DISK_RW),format=raw -device nvme,serial=godeep0,drive=disk0
# USB: an xHCI controller with a keyboard and a mouse (headless, they get
# input from QMP input-send-event; see check-virt).
QEMU_DISK   += -device qemu-xhci,id=xhci -device usb-kbd,bus=xhci.0 -device usb-mouse,bus=xhci.0 -device usb-tablet,bus=xhci.0,port=4
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
# the guest's port 80 (httpd) on the host
HTTP_PORT   ?= 8080
QEMU_NET    = -device ramfb -global virtio-mmio.force-legacy=false -device virtio-rng-device -device virtio-gpu-device,id=gpu0 \
	-netdev user,id=net0,hostfwd=tcp:127.0.0.1:$(SSH_PORT)-:22,hostfwd=tcp:127.0.0.1:$(HTTP_PORT)-:80 -device virtio-net-device,netdev=net0 \
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
# NOGC=1: the kernel never runs the garbage collector (debug.SetGCPercent(-1)
# before anything else; cmd/godeep). The heap only grows: until the board's
# RAM runs out, then the runtime dies with "out of memory".
ifneq ($(NOGC),)
TAMAGO_LDFLAGS += -X main.noGC=1
endif
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

.PHONY: check-crash check-desktop efiboot parallels-setup parallels check-parallels ssh-parallels wasm push hot check-usbnet check-pcie check-iommu check-hotplug check-linklocal test hosted kernel image qemu check-virt qemu-applem1 shot-applem1 check-applem1 native stop kill clean keys ssh

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
	$(TAMAGO_ENV) $(TAMAGO) build -tags "$(TAMAGO_TAGS)$(GODEEP_RUNTIME_TAG)" -trimpath -ldflags "$(TAMAGO_LDFLAGS)" -o $(KERNEL) ./cmd/godeep

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
check-virt: HTTP_PORT = 8083
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
	  -step 'mkdir nvme0p2:/fromgodeep; yes ext4 | head -n 5000 > nvme0p2:/fromgodeep/lines.txt=>' \
	  -step 'wc -l < nvme0p2:/fromgodeep/lines.txt=>5000' -step 'rm nvme0p2:/fromgodeep/lines.txt nvme0p2:/fromgodeep=>' \
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
	  -step 'lsdev xhci=>HID tablet' -step 'send:evtest 3' -step 'wait:listening' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"abs","data":{"axis":"x","value":16384}},{"type":"abs","data":{"axis":"y","value":8192}}]}}' \
	  -step 'wait:absolute x=16384 y=8192' -step 'lsdev fbcon=>pointer at 639,199' \
	  -step 'lsdev gpu0=>scanout 0,ready' -step 'gpu test=>gpu0: showing' \
	  -step 'qmp:{"execute":"screendump","arguments":{"filename":"bin/check-gpu.png","device":"gpu0","format":"png"}}' \
	  -step 'local:go run ./tools/pngpix -bars 8 bin/check-gpu.png=>#ffffff #ffff00 #00ffff #00ff00 #ff00ff #ff0000 #0000ff #000000' \
	  -step 'lsdev rng0=>seeded crypto/rand' \
	  -step 'ifconfig=>10.0.2.15/24,52:54:00,dhcp from 10.0.2.2,dns 10.0.2.3' \
	  -step 'get http://10.0.2.2:2280/go.mod=>module godeep' \
	  -step 'ping -c 2 -i 0.2 10.0.2.2=>2 received' \
	  -step 'echo GET /go.mod HTTP/1.0 > ram:/req; echo >> ram:/req=>' \
	  -step 'nc 10.0.2.2 2280 < ram:/req | grep ^module=>module godeep' \
	  -step "echo 'N=\$$1; test -e ram:/req && echo have \$$N, \$$# arg' > ram:/t.sh=>" \
	  -step 'source ram:/t.sh it=>have it, 1 arg' \
	  -step 'false || echo status-$$?=>status-1' \
	  -step 'sleep 0.3 & wait; echo waited-$$?=>waited-0' \
	  -step 'restrict -time 300ms sleep 5=>resource limit: time 300ms' \
	  -step 'restrict -out 100 yes | wc=>50 50 100' \
	  -step 'echo from-godeep-httpd > ram:/web.txt=>' -step 'send:httpd &' -step 'wait:serving' \
	  -step 'local:curl -s http://127.0.0.1:$(HTTP_PORT)/ram/web.txt=>from-godeep-httpd' \
	  -step 'send:edit ram:/ed.txt' -step 'wait:new file' -step 'raw:typed in edit\x1b[D\x1b[D!\x13\x11' \
	  -step 'cat ram:/ed.txt=>typed in ed!it' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 ps=>sshd,netd,PID' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 lsdev net0=>virtio,device1,ready,rx' \
	  -step 'local:echo uptime | ssh $(SSH_OPTS) -tt root@127.0.0.1=>godeep$$ ' \
	  -step 'lsdev net0=>tx' \
	  -step 'local:scp $(SCP_OPTS) bin/wasm/hello.wasm root@127.0.0.1:=>' \
	  -step 'df=>ram,memfs,rw,nvme0p2,ext4,rw' \
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
	  -step 'ntp 10.0.2.2:2123=>clock corrected' \
	  -step 'date=>2030,ntp 10.0.2.2:2123' \
	  -step 'yes pipe | head -3 | wc=>3 3 15' \
	  -step 'lsdev | grep -i nvme | sort -r > ram:/devs=>' -step 'cat ram:/devs=>QEMU NVMe Ctrl' \
	  -step 'grep name < proc:/1/status=>devmgr' -step 'cat sys:/nvme0/state=>ready' \
	  -step 'send:kvd &' -step 'wait:] kvd' -step 'kv set os goroutines=>' -step 'kv get os=>goroutines' \
	  -step 'ports=>KVReq' -step 'top -n 1 -d 0.5=>%RUN,kvd,GOROUTINES' \
	  -step 'restrict -vol ram -ro write ram:/x hi=>read-only' \
	  -step 'restrict -vol ram cat nvme0p2:/hello.txt=>no such volume' \
	  -step 'local:ssh $(SSH_OPTS) root@127.0.0.1 "ls sys:/ | grep net"=>net0' \
	  -qmp bin/qmp-check.sock -http 127.0.0.1:2280=. -ninep 127.0.0.1:5641=bin/ninep-check -ntp 127.0.0.1:2123 \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_DISK) $(QEMU_NET) -qmp unix:bin/qmp-check.sock,server=on,wait=off -kernel $(BOOTFILE)
	go run ./tools/fsckpart $(DISK_RW) 2
	go run ./tools/fsckpart $(DISK_RW) 1
	@$(MAKE) -s check-usbnet
	@$(MAKE) -s check-uas
	@$(MAKE) -s check-smp SMP=4
	@$(MAKE) -s check-pcie
	@$(MAKE) -s check-iommu
	@$(MAKE) -s check-hotplug
	@$(MAKE) -s check-linklocal
	@$(MAKE) -s check-kexec
	@$(MAKE) -s check-wm
	@$(MAKE) -s check-desktop
	@$(MAKE) -s check-crash

# USB Ethernet (CDC ECM) as the only network: QEMU's usb-net on the xHCI,
# DHCP and SSH over it. The Mac's USB-C gadget link will look like this
# from godeep's side. Run by check-virt.
# USB Attached SCSI: QEMU's usb-uas on a USB 2 port (no bulk streams), a
# copy of the test disk behind it: partitions, ext4 and FAT reads, a FAT
# write read back (Write Ready IUs). USB-C SSDs speak UAS. Run by check-virt.
check-uas:
	@$(MAKE) -s image $(DISK)
	@cp $(DISK) bin/disk-uas.img
	go run ./tools/qemucheck -log bin/qemucheck-uas.log \
	  -step 'lsdev xhci=>mass storage (UAS)' -step 'lsdev usbdisk0=>UAS,ready' \
	  -step 'lsblk=>usbdisk0p1,FAT32,usbdisk0p2,ext4' \
	  -step 'cat usbdisk0p2:/hello.txt=>hello from godeep' \
	  -step 'yes uas-write-check | head -500 > usbdisk0p1:/uas.txt=>' \
	  -step 'wc -l < usbdisk0p1:/uas.txt=>500' -step 'tail -1 < usbdisk0p1:/uas.txt=>uas-write-check' \
	  -- $(QEMU) $(QEMU_FLAGS) -device qemu-xhci,id=xhci,p2=8 \
	  -device usb-uas,id=uas,bus=xhci.0,port=5 -drive if=none,id=uasdisk,file=bin/disk-uas.img,format=raw \
	  -device scsi-hd,bus=uas.0,scsi-id=0,lun=0,drive=uasdisk \
	  -kernel $(BOOTFILE)

# SMP: QEMU virt with SMP cores (4 when run by check-virt). Every core is
# started (board/virt/smp.go), a CPU-bound program on as many goroutines
# makes QEMU use more than (cores - 1.5) host cores, and the shell still
# answers under that load.
# CPU faults in a goroutine panic in it (third_party/tamago-go README: CPU
# faults as panics): each kind of fault kills only its process, again and
# again; a program recovers from one; 8000 recovered with GC running; then
# timers and the shell still work. Needs bin/tamago-go (make tamago-go).
check-crash:
	@$(MAKE) -s image
	@test -n "$(GODEEP_RUNTIME_TAG)" || { echo "check-crash: needs bin/tamago-go with the runtime patch (make tamago-go)"; exit 1; }
	go run ./tools/qemucheck -log bin/qemucheck-crash.log -timeout 120s \
	  -step 'crash panic=>panic: crash: on purpose' -step 'crash nil=>nil pointer dereference' \
	  -step 'crash nil=>nil pointer dereference' -step 'crash write=>nil pointer dereference' \
	  -step 'crash leaf=>nil pointer dereference' -step 'crash frame=>nil pointer dereference' \
	  -step 'crash recover && echo rc0=>rc0' -step 'crash child=>nil pointer dereference' \
	  -step 'crash storm=>4000 faults recovered' -step 'sleep 1 && echo slept=>slept' -step 'ps=>init' \
	  -- $(QEMU) -name godeep-crash $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -kernel $(BOOTFILE)

check-smp:
	@$(MAKE) -s image
	go run ./tools/qemucheck -log bin/qemucheck-smp.log \
	  -step 'dmesg=>$(SMP) cores' -step 'cat proc:/version=>GOMAXPROCS=$(SMP)' \
	  -step 'send:burn $(SMP) 10 &' -step 'wait:] burn' \
	  -step 'local:sleep 1; tools/cpuload.sh godeep-smp 4 $(shell echo "$(SMP) - 1.5" | bc)=>parallel' \
	  -step 'uptime=>up' -step 'wait:MB/s' \
	  -- $(QEMU) -name godeep-smp $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -kernel $(BOOTFILE)

# kexec: boot the same Image again from the running kernel (read over the
# 9p host share), without QEMU's help: the second boot finds the first's
# clean halt in pstore, and its devices (virtio-net, 9p) work again.
check-kexec:
	@$(MAKE) -s image
	go run ./tools/qemucheck -log bin/qemucheck-kexec.log \
	  -step 'lsdev net0=>ready' -step 'cat host:/go.mod=>module godeep' \
	  -step 'send:kexec host:/$(BOOTFILE)' -step 'wait:kexec: booting' -step 'wait:godeep 0.1.0' \
	  -step 'dmesg=>pstore: boot 2, the previous boot halted cleanly' \
	  -step 'lsdev net0=>ready' -step 'cat host:/go.mod=>module godeep' \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_NET) -kernel $(BOOTFILE)

# The compositor (kernel/wm) on virtio-gpu: three windows come up where
# the cascade puts them, and the pattern window follows a drag of its title
# bar through the USB tablet (QMP; pauses between steps, since the tablet's
# position and the mouse's button reach wm from different devices).
# The desktop (programs/sh/desktop.go) on QEMU's virtio-gpu: one terminal
# window, Night theme: the gradient's bottom, the focused title bar and the
# terminal's background in a screendump (window at 36,64 on 1280x800).
check-desktop:
	@$(MAKE) -s image
	@rm -f bin/qmp-desktop.sock
	go run ./tools/qemucheck -log bin/qemucheck-desktop.log -qmp bin/qmp-desktop.sock \
	  -step 'send:desktop -n 1 -t 60 &' -step 'local:sleep 2=>' \
	  -step 'ps=>term1' \
	  -step 'qmp:{"execute":"screendump","arguments":{"filename":"bin/check-desktop.png","device":"gpu0","format":"png"}}' \
	  -step 'local:go run ./tools/pngpix bin/check-desktop.png 5,795 400,50 300,400=>#10111a #24283b #1a1b26' \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_NET) -device qemu-xhci,id=xhci -device usb-tablet,bus=xhci.0,port=4 \
	  -qmp unix:bin/qmp-desktop.sock,server=on,wait=off -kernel $(BOOTFILE)

check-wm:
	@$(MAKE) -s image
	@rm -f bin/qmp-wm.sock
	go run ./tools/qemucheck -log bin/qemucheck-wm.log -qmp bin/qmp-wm.sock \
	  -step 'send:wm 60 &' -step 'wait:pattern at 120,138' -step 'local:sleep 1=>' \
	  -step 'qmp:{"execute":"screendump","arguments":{"filename":"bin/check-wm.png","device":"gpu0","format":"png"}}' \
	  -step 'local:go run ./tools/pngpix bin/check-wm.png 140,188 180,188 20,20=>#ffffff #ffff00 #2b3a4a' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"abs","data":{"axis":"x","value":5124}},{"type":"abs","data":{"axis":"y","value":5290}}]}}' -step 'local:sleep 0.3=>' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":true,"button":"left"}}]}}' -step 'local:sleep 0.3=>' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"abs","data":{"axis":"x","value":10000}},{"type":"abs","data":{"axis":"y","value":5290}}]}}' -step 'local:sleep 0.3=>' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"abs","data":{"axis":"x","value":15371}},{"type":"abs","data":{"axis":"y","value":5290}}]}}' -step 'local:sleep 0.3=>' \
	  -step 'qmp:{"execute":"input-send-event","arguments":{"events":[{"type":"btn","data":{"down":false,"button":"left"}}]}}' -step 'local:sleep 0.3=>' \
	  -step 'qmp:{"execute":"screendump","arguments":{"filename":"bin/check-wm2.png","device":"gpu0","format":"png"}}' \
	  -step 'local:go run ./tools/pngpix bin/check-wm2.png 540,188 140,320=>#ffffff #2b3a4a' \
	  -- $(QEMU) $(QEMU_FLAGS) $(QEMU_NET) -device qemu-xhci,id=xhci -device usb-mouse,bus=xhci.0 -device usb-tablet,bus=xhci.0,port=4 \
	  -qmp unix:bin/qmp-wm.sock,server=on,wait=off -kernel $(BOOTFILE)

check-usbnet:
	@$(MAKE) -s image
	go run ./tools/qemucheck -log bin/qemucheck-usbnet.log \
	  -step 'lsdev usbnet=>usbnet0,godeep,usb-ecm,ready' -step 'ifconfig=>dhcp from 10.0.2.2' \
	  -step 'local:ssh -i $(SSH_DIR)/id_ed25519 -p 2224 -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@127.0.0.1 lsdev usbnet0=>tx' \
	  -- $(QEMU) $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -device qemu-xhci,id=xhci -netdev user,id=u0,hostfwd=tcp:127.0.0.1:2224-:22 -device usb-net,netdev=u0,bus=xhci.0 \
	  -kernel $(BOOTFILE)

# PCIe bridges: the NVMe disk behind a root port (QEMU's pcie-root-port), the
# way the Mac's PCIe devices sit behind its root ports; and an SD card in an
# SDHCI reader behind another (the Mac's card reader is one), read and written.
check-pcie:
	@$(MAKE) -s image $(DISK)
	@cp $(DISK) bin/sd-check.img
	@go run ./tools/mkdisk -mac -size 134217728 -put godeep/rc=tools/mkdisk/testdata/rc -o bin/mac-check.img >/dev/null
	@rm -f bin/qmp-pcie.sock
	go run ./tools/qemucheck -log bin/qemucheck-pcie.log -qmp bin/qmp-pcie.sock \
	  -step 'wait:rc ran, status 0' -step 'cat ram:/rc-devs=>godeep-mac' \
	  -step 'dmesg=>confd: source nvme1p2:/godeep/rc,pstore: boot' \
	  -step 'lsdev pci=>bridge0,bridge to buses 1-1,nvme0 01:00.0,nvme1 03:00.0' \
	  -step 'lsblk=>nvme0p1,FAT32,nvme0p2,ext4' \
	  -step 'cat nvme0p2:/hello.txt=>hello from godeep' \
	  -step 'lsdev mmc=>SDSC card,64 MB,4-bit at' \
	  -step 'lsblk=>mmc0p1,FAT32,mmc0p2,ext4' \
	  -step 'cat mmc0p2:/hello.txt=>hello from godeep' \
	  -step 'cat mmc0p1:/many/f123.txt=>file 123' \
	  -step 'write mmc0p1:/sd.txt written on the SD card=>' \
	  -step 'cat mmc0p1:/sd.txt=>written on the SD card' \
	  -step 'lsdev nvme1=>godeep-mac,writes only to p2' \
	  -step 'write nvme1p2:/fenced.txt inside the fence=>' \
	  -step 'cat nvme1p2:/fenced.txt=>inside the fence' \
	  -step 'write nvme1p3:/fenced.txt outside the fence=>read-only' \
	  -step 'ls nvme1p3:/=>docs' \
	  -step 'dmesg=>logd: saving the kernel log to nvme1p2' \
	  -step 'ls nvme1p2:/godeep=>log.txt' \
	  -step 'echo last-words-before-reset=>last-words' \
	  -step 'qmp:{"execute":"system_reset"}' -step 'wait:godeep 0.1.0' \
	  -step 'dmesg=>pstore: the previous boot ended without a clean halt' \
	  -step 'grep -c last-words-before-reset < ram:/crash.txt=>2' \
	  -step 'dmesg=>crashed boot' -step 'cat nvme1p2:/godeep/crash.txt | grep -c last-words=>2' \
	  -- $(QEMU) $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -qmp unix:bin/qmp-pcie.sock,server=on,wait=off -action reboot=reset \
	  -device pcie-root-port,id=rp1,bus=pcie.0,chassis=1 \
	  -drive if=none,id=d0,file=$(DISK),format=raw,readonly=on -device nvme,serial=godeep1,drive=d0,bus=rp1 \
	  -device pcie-root-port,id=rp2,bus=pcie.0,chassis=2 -device sdhci-pci,id=sd0,bus=rp2 \
	  -drive if=none,id=sdcard,file=bin/sd-check.img,format=raw -device sd-card,drive=sdcard \
	  -device pcie-root-port,id=rp3,bus=pcie.0,chassis=3 \
	  -drive if=none,id=mac,file=bin/mac-check.img,format=raw -device nvme,serial=godeep-mac,drive=mac,bus=rp3 \
	  -kernel $(BOOTFILE)
	@LC_ALL=C grep -aq 'godeep: halt, stopping all processes' bin/mac-check.img && \
	  echo "check-pcie: OK: the kernel log reached GODEEPDATA, halt included" || \
	  { echo "check-pcie: FAIL: the log on GODEEPDATA lacks the halt"; exit 1; }

# Every PCIe device behind an IOMMU (QEMU's SMMUv3), set up like the Mac's
# DARTs: NVMe, xHCI with a USB stick and USB Ethernet all DMA through
# translated bus addresses. A driver that hands a device a physical address
# fails here (and lsdev pci lists the fault).
check-iommu:
	@$(MAKE) -s image $(DISK)
	go run ./tools/qemucheck -log bin/qemucheck-iommu.log \
	  -step 'lsdev pci=>SMMUv3: DMA region at bus 0x10000000,nvme0,xhci0' \
	  -step 'lsblk=>nvme0p1,FAT32,nvme0p2,ext4,usbdisk0p1' \
	  -step 'cat nvme0p2:/hello.txt=>hello from godeep' \
	  -step 'cat usbdisk0p2:/hello.txt=>hello from godeep' \
	  -step 'lsdev xhci=>HID boot keyboard,hub,mass storage,ethernet' \
	  -step 'ifconfig=>dhcp from 10.0.2.2' \
	  -step 'local:ssh -i $(SSH_DIR)/id_ed25519 -p 2225 -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@127.0.0.1 lsdev pci=>SMMUv3' \
	  -- $(QEMU) $(QEMU_FLAGS) -machine iommu=smmuv3 -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -drive if=none,id=d0,file=$(DISK),format=raw,readonly=on -device nvme,serial=godeep-iommu,drive=d0 \
	  -device qemu-xhci,id=xhci -device usb-kbd,bus=xhci.0 -device usb-hub,bus=xhci.0,port=3 \
	  -drive if=none,id=stick,file=$(DISK),format=raw,readonly=on -device usb-storage,bus=xhci.0,port=3.1,drive=stick \
	  -netdev user,id=u0,hostfwd=tcp:127.0.0.1:2225-:22 -device usb-net,netdev=u0,bus=xhci.0 \
	  -kernel $(BOOTFILE)

# USB hot-plug: a stick and the USB Ethernet adapter pulled and plugged back
# over QMP. Unplugged devices leave lsblk/lsdev; back, they get their old
# names (usbdisk0, usbnet0) and netd carries on over the adapter.
check-hotplug:
	@$(MAKE) -s image $(DISK)
	go run ./tools/qemucheck -log bin/qemucheck-hotplug.log -qmp bin/qmp-hotplug.sock \
	  -step 'cat usbdisk0p2:/hello.txt=>hello from godeep' \
	  -step 'ifconfig=>dhcp from 10.0.2.2' \
	  -step 'qmp:{"execute":"device_del","arguments":{"id":"stick"}}' \
	  -step 'lsblk=>!usbdisk0' \
	  -step 'cat usbdisk0p2:/hello.txt=>no such volume' \
	  -step 'qmp:{"execute":"device_add","arguments":{"driver":"usb-storage","id":"stick","bus":"xhci.0","port":"2","drive":"stick"}}' \
	  -step 'cat usbdisk0p2:/hello.txt=>hello from godeep' \
	  -step 'qmp:{"execute":"device_del","arguments":{"id":"nic"}}' \
	  -step 'lsdev usbnet=>!usbnet0' \
	  -step 'qmp:{"execute":"device_add","arguments":{"driver":"usb-net","id":"nic","bus":"xhci.0","port":"3","netdev":"u0"}}' \
	  -step 'lsdev usbnet=>usbnet0,ready' \
	  -step 'local:ssh -i $(SSH_DIR)/id_ed25519 -p 2226 -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@127.0.0.1 lsdev usbnet0=>tx' \
	  -- $(QEMU) $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -device qemu-xhci,id=xhci \
	  -blockdev driver=file,node-name=stickfile,filename=$(DISK),read-only=on -blockdev driver=raw,node-name=stick,file=stickfile,read-only=on \
	  -device usb-storage,id=stick,bus=xhci.0,port=2,drive=stick \
	  -netdev user,id=u0,hostfwd=tcp:127.0.0.1:2226-:22 -device usb-net,id=nic,bus=xhci.0,port=3,netdev=u0 \
	  -qmp unix:bin/qmp-hotplug.sock,server=on,wait=off \
	  -kernel $(BOOTFILE)

# No DHCP on the wire (a NIC on a socket nobody's on: like a cable to a
# computer without a DHCP server): netd falls back to a link-local address.
check-linklocal:
	@$(MAKE) -s image
	go run ./tools/qemucheck -log bin/qemucheck-linklocal.log \
	  -step 'ifconfig=>169.254.,link-local (DHCP failed)' \
	  -step 'dmesg=>DHCP retried every' \
	  -- $(QEMU) $(QEMU_FLAGS) -global virtio-mmio.force-legacy=false -device virtio-rng-device \
	  -netdev socket,id=n0,listen=127.0.0.1:2291 -device virtio-net-device,netdev=n0 \
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
	bin/fakem1n1-check check -bootargs godeep.nodesktop
	bin/fakem1n1-check check -fault 1 -o bin/fakem1n1/fault1
	bin/fakem1n1-check check -fault 3 -o bin/fakem1n1/fault3
	bin/fakem1n1-check check -o bin/fakem1n1/desktop -want "board:,FEATURE,smp,agx" -type ""
	bin/fakem1n1-check check -smp 10 -bootargs godeep.nodesktop -o bin/fakem1n1/enable -type "$$(printf 'enable smp agx\r')" -reply "smp: 10 of 10 cores"
	bin/fakem1n1-check check -smp 10 -bootargs "godeep.smp godeep.nodesktop" -o bin/fakem1n1/smp10 -want "board:,10 of 10 cores,uptime,wake-ups/s"
	bin/fakem1n1-check check -smp 10 -secmmu -bootargs "godeep.smp godeep.nodesktop" -o bin/fakem1n1/secmmu -want "board:,10 of 10 cores,uptime"
	bin/fakem1n1-check check -smp 4 -park wfi -bootargs "godeep.smp godeep.nodesktop" -o bin/fakem1n1/wfi -want "board:,1 of 4 cores,didn't start,uptime"

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
	BOOTARGS="$(BOOTARGS)" tools/godeep-partition.sh install $(if $(DRY),--dry-run)

# The patched TamaGo toolchain (about a minute): a shared clone of
# $(TAMAGO_SRC) at the same commit, the patch, make.bash. It won't touch an
# existing bin/tamago-go: delete that first to rebuild.
tamago-go:
	@test ! -e bin/tamago-go || { echo "bin/tamago-go exists: delete it to rebuild"; exit 1; }
	@mkdir -p bin && echo "module godeep-bin // keeps bin/ (this toolchain's sources) out of godeep's ./..." > bin/go.mod
	git clone -q --shared $(TAMAGO_SRC) bin/tamago-go
	git -C bin/tamago-go -c advice.detachedHead=false checkout -q $$(git -C $(TAMAGO_SRC) rev-parse HEAD)
	git -C bin/tamago-go apply $(CURDIR)/third_party/tamago-go/godeep-runtime.patch
	cd bin/tamago-go/src && ./make.bash >/dev/null
	@echo "bin/tamago-go: TamaGo with godeep's runtime patch (now the default TAMAGO)"

# Parallels Desktop (docs/Parallels.md): the same arm64 Image, loaded by
# tools/efiboot (a UEFI app: clang --target=aarch64-windows + lld-link,
# `brew install lld`) with a device tree from tools/efiboot/mkdtb. PD_VM is
# an arm64 VM ("Other" OS, 4 GB: RAM must be 0x40000000-0x13fffffff);
# parallels-setup gives it bin/parallels/boot.hdd (a plain FAT disk) first in
# the boot order and its first serial port on bin/parallels/serial.sock.
PD_VM   ?= GoDeep
PD_CPUS ?= 4
PD_DISK  = bin/parallels/boot.hdd
EFI_CC   = clang --target=aarch64-windows -ffreestanding -fno-stack-protector -fno-builtin -mgeneral-regs-only -O2 -Wall

bin/parallels/BOOTAA64.EFI: tools/efiboot/efiboot.c tools/efiboot/efi.h
	@mkdir -p bin/parallels
	$(EFI_CC) -c tools/efiboot/efiboot.c -o bin/parallels/efiboot.obj
	lld-link /subsystem:efi_application /entry:efi_main /nodefaultlib /machine:arm64 /out:$@ bin/parallels/efiboot.obj >/dev/null

efiboot: bin/parallels/BOOTAA64.EFI

parallels-setup:
	@mkdir -p bin/parallels
	@test -d $(PD_DISK) || { prl_disk_tool create --hdd $(PD_DISK) --size 64M >/dev/null && \
	  mformat -i "$$(ls $(PD_DISK)/*.hds)" -F -v GODEEPBOOT :: && mmd -i "$$(ls $(PD_DISK)/*.hds)" ::/EFI ::/EFI/BOOT; }
	-prlctl stop $(PD_VM) --kill >/dev/null 2>&1
	prlctl set $(PD_VM) --cpus $(PD_CPUS) --memsize 4096 --on-window-close keep-running
	@prlctl list -i $(PD_VM) | grep -q "$(CURDIR)/$(PD_DISK)" || prlctl set $(PD_VM) --device-add hdd --image $(CURDIR)/$(PD_DISK)
	@prlctl list -i $(PD_VM) | grep -q "serial0 " || prlctl set $(PD_VM) --device-add serial --socket $(CURDIR)/bin/parallels/serial.sock --socket-mode server
	prlctl set $(PD_VM) --device-set serial0 --socket $(CURDIR)/bin/parallels/serial.sock --socket-mode server
	prlctl set $(PD_VM) --device-bootorder "$$(prlctl list -i $(PD_VM) | awk -v d='$(CURDIR)/$(PD_DISK)' 'index($$0, d) {print $$1}')"

# Build, copy onto the boot disk, (re)start the VM. Serial console: connect
# to bin/parallels/serial.sock (e.g. `nc -U bin/parallels/serial.sock`).
parallels: image efiboot
	go run ./tools/efiboot/mkdtb -cpus $(PD_CPUS) -o bin/parallels/parallels.dtb
	-prlctl stop $(PD_VM) --kill >/dev/null 2>&1
	mcopy -o -i "$$(ls $(PD_DISK)/*.hds)" bin/parallels/BOOTAA64.EFI ::/EFI/BOOT/BOOTAA64.EFI
	mcopy -o -i "$$(ls $(PD_DISK)/*.hds)" $(BOOTFILE) ::/godeep.img
	mcopy -o -i "$$(ls $(PD_DISK)/*.hds)" bin/parallels/parallels.dtb ::/parallels.dtb
	prlctl start $(PD_VM)

# The VM's address on Parallels' shared network (10.211.55.x): the newest
# DHCP lease for its MAC. godeep answers ssh there directly, no forwarding.
PD_IP = $$(mac=$$(prlctl list -i $(PD_VM) | sed -n 's/.*net0 .*mac=\([0-9A-Fa-f]*\).*/\1/p' | tr A-F a-f); \
	grep "$$mac" /Library/Preferences/Parallels/parallels_dhcp_leases | tail -1 | cut -d= -f1)
PD_SSH  = ssh $(subst -p $(SSH_PORT),-p 22,$(SSH_OPTS))

# make ssh-parallels [CMD='lsdev net0']
ssh-parallels: $(SSH_DIR)/id_ed25519
	$(PD_SSH) root@$(PD_IP) $(CMD)

# Boot on Parallels and check every core runs: GOMAXPROCS, all cores
# started, and a CPU-bound program on PD_CPUS goroutines uses more than
# (PD_CPUS - 0.5) host cores; then the network: a DHCP lease, and ssh from
# this Mac to the VM's address.
check-parallels: parallels
	go run ./tools/pdcheck -vm $(PD_VM) -sock bin/parallels/serial.sock \
	  -step 'cat proc:/version=>GOMAXPROCS=$(PD_CPUS)' -step 'lsdev cpus=>$(PD_CPUS) cores' \
	  -step 'load:burn $(PD_CPUS) 10=>$(shell echo "$(PD_CPUS) - 0.5" | bc)' -step 'uptime=>up' \
	  -step 'ifconfig=>dhcp from'
	@for i in 1 2 3 4 5 6 7 8 9 10; do \
	  out=$$($(PD_SSH) -o ConnectTimeout=3 root@$(PD_IP) cat proc:/version 2>&1) && break; sleep 2; done; \
	  echo "$$out" | grep -q "GOMAXPROCS=$(PD_CPUS)" && echo "check-parallels: OK: ssh root@$(PD_IP)" || \
	  { echo "check-parallels: FAIL: ssh root@$(PD_IP): $$out"; exit 1; }

stop:
	@tools/qemu-stop.sh

kill:
	@tools/qemu-stop.sh --force

clean:
	rm -rf bin
