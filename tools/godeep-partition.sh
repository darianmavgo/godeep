#!/bin/bash
# godeep-partition: install godeep next to Macintosh HD and Omarchy.
#
#   tools/godeep-partition.sh            install (shrink macOS if needed, then
#                                        create the "godeep" boot entry; the
#                                        Asahi installer's menus are answered
#                                        for you)
#   tools/godeep-partition.sh finish     after the reboot: add GODEEPDATA, build
#                                        (if needed) and make m1n1 boot
#                                        bin/godeep-applem1.img directly, in
#                                        place of U-Boot (rerun to update godeep)
#   tools/godeep-partition.sh install    only (re)install the godeep image
#   --uboot                              keep U-Boot: it autoboots godeep via
#                                        extlinux.conf (but on a Mac with Omarchy
#                                        it may boot Omarchy instead)
#   --no-autoboot                        keep U-Boot and let it stop at its
#                                        prompt (boot godeep by hand)
#   tools/godeep-partition.sh status     show where things stand (read-only)
#   tools/godeep-partition.sh undo       remove godeep and give the space back
#   --dry-run                            show every command, change nothing
#   GODEEP_GB=20 tools/godeep-partition.sh   use 20 GB instead of 12
#
# What it does is documented step by step in docs/TargetMyMac.md §6.
# Every change asks for confirmation; nothing touches Omarchy or recoveryOS.

set -euo pipefail

GODEEP_GB="${GODEEP_GB:-12}"
DRY=0
AUTOBOOT=1
DIRECT=1 # m1n1 boots godeep itself (m1n1/boot.bin's U-Boot payload replaced)
CMD="setup"
for a in "$@"; do
	case "$a" in
	--dry-run) DRY=1 ;;
	--uboot) DIRECT=0 ;;
	--no-autoboot) DIRECT=0; AUTOBOOT=0 ;;
	setup | status | undo | install) CMD="$a" ;;
	finish | data) CMD=finish ;;
	-h | --help) sed -n '2,25p' "$0"; exit 0 ;;
	*) echo "unknown argument: $a (try --help)"; exit 2 ;;
	esac
done

bold=$'\033[1m'; red=$'\033[31m'; grn=$'\033[32m'; ylw=$'\033[33m'; off=$'\033[0m'
say()  { printf '%s\n' "$*"; }
ok()   { printf '%s✓%s %s\n' "$grn" "$off" "$*"; }
warn() { printf '%s!%s %s\n' "$ylw" "$off" "$*"; }
die()  { printf '%s✗ %s%s\n' "$red" "$*" "$off" >&2; exit 1; }
step() { printf '\n%s== %s ==%s\n' "$bold" "$*" "$off"; }

run() { # run a command that changes the disk (or show it in --dry-run)
	printf '  %s$ %s%s\n' "$bold" "$*" "$off"
	if [ "$DRY" = 1 ]; then say "  (dry run: not executed)"; return 0; fi
	"$@"
}

confirm() { # confirm "question" -> requires typing "godeep"
	if [ "$DRY" = 1 ]; then say "  (dry run: would ask: $1)"; return 0; fi
	printf '\n%s%s%s\n  Type %sgodeep%s to continue, anything else to stop: ' "$bold" "$1" "$off" "$bold" "$off"
	read -r answer </dev/tty
	[ "$answer" = "godeep" ] || die "stopped; nothing was changed by this step"
}

pause() {
	[ "$DRY" = 1 ] && return 0
	printf '\n%s' "$1"; read -r _ </dev/tty
}

plist() { plutil -extract "$1" raw - 2>/dev/null; }
gb() { awk -v b="$1" 'BEGIN { printf "%.1f GB", b / 1e9 }'; }

# ---------- facts about this Mac ----------

[ "$(uname -s)" = Darwin ] || die "run this on macOS"
[ "$(uname -m)" = arm64 ] || die "this is for Apple Silicon Macs"

CONTAINER=$(diskutil info -plist / | plist APFSContainerReference) || die "can't find the macOS container"
STORE=$(diskutil info -plist / | plist APFSPhysicalStores.0.APFSPhysicalStore) || die "can't find the macOS partition"
DISK=${STORE%s*}
REPO=$(cd "$(dirname "$0")/.." && pwd)
# The Mac build (BOARD=applem1), not bin/godeep-arm64.img (that one is QEMU's).
IMAGE="$REPO/bin/godeep-applem1.img"

layout() { diskutil list "$DISK"; }
has() { layout | grep -q "$1"; }
godeep_esp() { layout | awk 'toupper($0) ~ /EFI - GODEE/ { print $NF }'; }
godeep_container() { # container whose volume is named "godeep"
	diskutil apfs list | awk '
		/APFS Container Reference:/ { c = $NF }
		/Name: +godeep \(/ { print c; exit }'
}
free_bytes() { # largest free-space gap on the disk (bytes, approx from diskutil)
	layout | awk '/\(free space\)/ {
		for (i = 1; i <= NF; i++) if ($i ~ /^[0-9.]+$/ && $(i+1) ~ /^(KB|MB|GB|TB)$/) {
			m = ($(i+1) == "KB") ? 1e3 : ($(i+1) == "MB") ? 1e6 : ($(i+1) == "GB") ? 1e9 : 1e12
			v = $i * m; if (v > max) max = v
		}
	} END { printf "%d", max + 0 }'
}
others_snapshot() { # every partition line except the macOS one, sizes included
	layout | grep -E '^ +[0-9]+:' | grep -v "$STORE\$" || true
}

show_status() {
	step "Disk layout ($DISK)"
	layout | sed 's/^/  /'
	say ""
	say "  macOS container:      $CONTAINER on $STORE"
	if has "EFI - OMARC"; then ok "Omarchy found (EFI - OMARC)"; else warn "no Omarchy ESP found"; fi
	local c esp
	c=$(godeep_container || true); esp=$(godeep_esp || true)
	if [ -n "$c" ]; then ok "godeep boot entry: stub container $c, ESP ${esp:-?}"; else say "  godeep boot entry:    not installed"; fi
	if has GODEEPDATA; then ok "GODEEPDATA partition present"; else say "  GODEEPDATA:           not created"; fi
	local f; f=$(free_bytes)
	if [ "$f" -gt 0 ]; then say "  free space on disk:   $(gb "$f")"; fi
}

# ---------- setup: shrink macOS, then the Asahi installer ----------

setup() {
	say "${bold}godeep partition setup${off}  (${GODEEP_GB} GB for godeep; GODEEP_GB=… to change)"
	[ "$DRY" = 1 ] && warn "dry run: nothing will be changed"

	step "1/4 Checks"
	if [ -n "$(godeep_container || true)" ]; then
		ok "the godeep boot entry already exists"
		say "  Next: tools/godeep-partition.sh finish   (adds GODEEPDATA, copies godeep), or: status"
		exit 0
	fi
	pmset -g batt | grep -q "AC Power" && ok "on AC power" || die "plug in the power adapter first"
	has "EFI - OMARC" && ok "Omarchy is present and will not be touched" || warn "no Omarchy found; continuing (godeep doesn't need it)"
	fdesetup status | grep -q "On" && ok "FileVault on (fine: you're logged in, the disk is unlocked)" || ok "FileVault off"
	if tmutil listlocalsnapshots / 2>/dev/null | grep -q "com.apple.TimeMachine"; then
		warn "Time Machine local snapshots exist; they can block shrinking."
		say "  If step 2 fails, run: tmutil thinlocalsnapshots / 999999999999 4"
	else
		ok "no Time Machine local snapshots"
	fi
	local last
	last=$(tmutil latestbackup 2>/dev/null || true)
	if [ -n "$last" ]; then
		ok "latest Time Machine backup: ${last##*/}"
	else
		warn "no Time Machine backup found. Back up first if you can: this repartitions your only disk."
	fi

	local limits cur minpref want existing_free
	limits=$(diskutil apfs resizeContainer "$CONTAINER" limits -plist)
	cur=$(printf '%s' "$limits" | plist CurrentSize)
	minpref=$(printf '%s' "$limits" | plist MinimumSizePreferred)
	existing_free=$(free_bytes)

	want=$((GODEEP_GB * 1000000000))
	if [ "$existing_free" -ge "$want" ]; then
		ok "there is already $(gb "$existing_free") of free space: no need to shrink macOS"
		SHRINK=0
	else
		SHRINK=1
		NEW=$(((cur - want + existing_free) / 4096 * 4096))
		[ "$NEW" -gt $((minpref + 20000000000)) ] || die "macOS can't shrink that far (minimum $(gb "$minpref") + 20 GB headroom)"
		ok "Macintosh HD: $(gb "$cur") now -> $(gb "$NEW") (minimum it could shrink to: $(gb "$minpref"))"
	fi

	step "2/4 Make room"
	if [ "$SHRINK" = 1 ]; then
		say "  This shrinks the Macintosh HD container by ${GODEEP_GB} GB. Your files are unaffected:"
		say "  the space comes from the container's unused part. The free gap appears right"
		say "  after Macintosh HD, i.e. between macOS and Omarchy."
		local before after
		before=$(others_snapshot)
		confirm "Shrink Macintosh HD by ${GODEEP_GB} GB?"
		run sudo diskutil apfs resizeContainer "$CONTAINER" "${NEW}B"
		if [ "$DRY" = 0 ]; then
			after=$(others_snapshot)
			[ "$before" = "$after" ] && ok "every other partition (Omarchy, recoveryOS, …) is unchanged" ||
				die "other partitions changed unexpectedly; stop and compare 'diskutil list $DISK' with docs/TargetMyMac.md §6.1"
			ok "free space now: $(gb "$(free_bytes)")"
		fi
	else
		ok "skipped"
	fi

	step "3/4 Create the godeep boot entry"
	cat <<EOF
  This uses the Asahi installer (the tool Omarchy used) to create a small boot
  container + boot partition named ${bold}godeep${off} in the free space. Its menus are
  answered for you; it only offers godeep (m1n1 + U-Boot + boot partition), and
  it never touches Macintosh HD or Omarchy.

  You'll be asked for:
    - ${bold}Enter${off} on its welcome screen
    - your ${bold}sudo password${off}, then your ${bold}macOS login password${off} (to approve the boot entry)
    - ${bold}Enter${off} to shut down when it's done

  After it shuts down: wait 25 seconds, ${bold}press and hold${off} the power button until
  "Loading startup options" appears, choose ${bold}godeep${off}, and follow the prompts.
  The installer prints these steps again just before shutting down.
EOF
	pause "  Press Enter to start (Ctrl-C to stop here)… "
	run_installer

	step "4/4 After the reboot"
	say "  Back in macOS, run:  ${bold}tools/godeep-partition.sh finish${off}"
	say "  It adds the GODEEPDATA partition and copies godeep onto the boot partition."
}

# ---------- the Asahi installer, answered for godeep ----------

ASAHI_CDN=https://cdn.asahilinux.org
ASAHI_DATA=https://github.com/AsahiLinux/asahi-installer-data/raw/prod/data/installer_data.json
ASAHI_DATA_ALT=https://alx.sh/installer_data.json

# godeep_auto.py runs the installer's own main.py, but first trims its OS list
# to one "godeep" entry (the UEFI-only boot chain) and answers its menus:
# "install into free space", that OS, and the name godeep. Any other question
# (e.g. passwords) is passed through to you. If the installer comes back to its
# main menu (meaning the install didn't happen), it quits instead of looping.
write_auto_py() {
	cat >"$1" <<'PY'
import json, logging, runpy, sys
import util

data = json.load(open("installer_data.json"))
uefi = [o for o in data["os_list"]
        if o["name"].startswith("UEFI environment only") and not o.get("expert")]
if len(uefi) != 1:
    sys.exit("godeep: this installer version has no single 'UEFI environment only' "
             "option; see docs/TargetMyMac.md §6.5 to do this step by hand")
godeep = dict(uefi[0], name="godeep (m1n1 + U-Boot + boot partition)",
              default_os_name="godeep")
json.dump({"os_list": [godeep]}, open("installer_data.json", "w"), indent=1)

ANSWERS = {"Action": "f", "OS": "1", "OS name": "godeep"}
answered = set()
real_prompt = util.input_prompt

def auto_prompt(*args):
    prompt = "".join(str(a) for a in args)
    key = prompt.split(" (")[0].rstrip(": ").strip()
    if key not in ANSWERS:
        return real_prompt(*args)
    if key in answered:
        if key == "Action":
            util.p_error("godeep: the installer is back at its main menu, so the install "
                         "didn't happen (see the messages above). Quitting.")
            return "q"
        return real_prompt(*args)
    answered.add(key)
    print(f"» {prompt}{ANSWERS[key]}   (answered for godeep)")
    logging.info(f"INPUT (godeep auto): {ANSWERS[key]!r}")
    return ANSWERS[key]

util.input_prompt = auto_prompt
sys.argv[0] = "main.py"
runpy.run_path("main.py", run_name="__main__")
PY
}

run_installer() {
	if [ "$DRY" = 1 ]; then
		say "  (dry run: would download the Asahi installer from $ASAHI_CDN and run it"
		say "   with the menus answered: install into free space, godeep, name 'godeep')"
		return 0
	fi
	local w ver
	w=$(mktemp -d /tmp/godeep-install.XXXXXX)
	say "  Downloading the installer into ${w}…"
	ver=$(curl -fsSL "$ASAHI_CDN/installer/latest") || die "can't reach $ASAHI_CDN (are you online?)"
	curl -fsSL -o "$w/installer.tar.gz" "$ASAHI_CDN/installer/installer-$ver.tar.gz" || die "download of installer $ver failed"
	curl -fsSL -o "$w/installer_data.json" "$ASAHI_DATA" ||
		curl -fsSL -o "$w/installer_data.json" "$ASAHI_DATA_ALT" || die "can't download installer_data.json"
	tar xf "$w/installer.tar.gz" -C "$w"
	ok "Asahi installer $ver"
	write_auto_py "$w/godeep_auto.py"
	sed 's/main\.py/godeep_auto.py/' "$w/install.sh" >"$w/godeep-install.sh"
	grep -q godeep_auto.py "$w/godeep-install.sh" || die "installer $ver changed its layout; see docs/TargetMyMac.md §6.5 to do this step by hand"
	# No REPORT/REPORT_TAG: nothing is sent to Asahi's install statistics.
	(cd "$w" && sudo -E env DISTRO=godeep DISTRO_DOCS="docs/TargetMyMac.md §6" \
		REPO_BASE="$ASAHI_CDN" INSTALLER_BASE="$ASAHI_CDN/installer" \
		caffeinate -dis sh ./godeep-install.sh) ||
		die "the installer stopped; its log is $w/installer.log"
	# Reaching here means it quit without shutting down.
	[ -n "$(godeep_container || true)" ] || die "the installer finished without creating godeep; its log is $w/installer.log"
}

# ---------- finish: GODEEPDATA + the godeep image ----------

finish() {
	local esp f mnt
	esp=$(godeep_esp || true)
	[ -n "$esp" ] || die "no godeep boot partition (EFI - GODEE…) yet: run tools/godeep-partition.sh first and finish the installer"

	step "GODEEPDATA partition"
	if has GODEEPDATA; then
		ok "already exists"
	else
		f=$(free_bytes)
		if [ "$f" -gt 500000000 ]; then
			say "  $(gb "$f") of FAT32 right after the godeep boot partition ($esp)"
			confirm "Create the GODEEPDATA partition?"
			run sudo diskutil addPartition "$esp" FAT32 GODEEPDATA 0
		else
			warn "no free space left for it; skipping (godeep doesn't need it to boot)"
		fi
	fi

	install_image "$esp"
	show_status
	boot_help "$esp"
}

# ---------- install: the Mac kernel image on godeep's boot partition ----------

image_stale() { # missing, or older than any Go/assembly source that goes into it
	[ ! -f "$IMAGE" ] && return 0
	[ -n "$(find "$REPO/kernel" "$REPO/board/applem1" "$REPO/drivers" "$REPO/programs" \
		"$REPO/cmd/godeep" "$REPO/third_party/tamago" -newer "$IMAGE" \
		\( -name '*.go' -o -name '*.s' \) 2>/dev/null | head -1)" ]
}

install_image() {
	local esp=$1 part mnt was_mounted
	[ "$esp" != "$(layout | awk '/EFI - OMARC/ { print $NF }')" ] || die "refusing: $esp is Omarchy's boot partition"
	part=${esp##*s} # GPT entry number = U-Boot's partition number (nvme 0:N)

	step "Build the Mac image (bin/godeep-applem1.img)"
	if image_stale; then
		say "  missing or older than the sources: building it (make image BOARD=applem1)"
		if [ "$DRY" = 1 ]; then
			say "  (dry run: not built)"
		else
			make -C "$REPO" -s image BOARD=applem1 || die "build failed: see CLAUDE.md for the TamaGo toolchain"
		fi
	fi
	if [ -f "$IMAGE" ]; then
		ok "$(basename "$IMAGE"): $(($(stat -f %z "$IMAGE") / 1024)) KB, built $(stat -f %Sm "$IMAGE")"
	fi

	step "Copy godeep onto its boot partition ($esp)"
	# m1n1/boot.bin is rebuilt on the host before anything is mounted, so a
	# failure here changes nothing. go runs as you, never under sudo.
	local tool="" newboot=""
	if [ "$DIRECT" = 1 ] && [ "$DRY" = 0 ]; then
		tool=$(mktemp -d /tmp/godeep-m1n1boot.XXXXXX)
		(cd "$REPO" && go build -o "$tool/m1n1boot" ./tools/m1n1boot) || die "can't build tools/m1n1boot"
	fi
	was_mounted=$(diskutil info -plist "$esp" | plist MountPoint || true)
	# EFI partitions need root to mount on current macOS.
	[ -n "$was_mounted" ] || run sudo diskutil mount "$esp"
	mnt=$(diskutil info -plist "$esp" | plist MountPoint || true)
	if [ "$DRY" = 1 ]; then
		say "  (dry run: would copy $IMAGE to ${mnt:-<boot partition>}/godeep.img"
		if [ "$DIRECT" = 1 ]; then
			say "   and replace the U-Boot payload in m1n1/boot.bin with godeep, keeping U-Boot's as m1n1/boot.bin.uboot)"
		elif [ "$AUTOBOOT" = 1 ]; then
			say "   and write extlinux/extlinux.conf so U-Boot boots it automatically)"
		fi
		return 0
	fi
	[ -n "$mnt" ] || die "couldn't mount $esp"
	local S="" # write as root if the mount isn't ours
	[ -w "$mnt" ] || S=sudo
	put() { $S tee "$1" >/dev/null; } # put FILE < content

	local boot="$mnt/m1n1/boot.bin" saved="$mnt/m1n1/boot.bin.uboot"
	if [ "$DIRECT" = 1 ]; then
		[ -f "$boot" ] || die "$esp has no m1n1/boot.bin: is it really the godeep boot partition?"
		# Build from U-Boot's boot.bin (saved the first time), so reruns don't stack payloads.
		local base="$saved" what
		if [ ! -f "$saved" ]; then
			base="$boot"
			what=$("$tool/m1n1boot" -info "$boot") || die "m1n1/boot.bin isn't in the format m1n1boot knows: not touching it"
			say "  $what"
			case "$what" in *", U-Boot") ;; *) die "m1n1/boot.bin's payload isn't U-Boot and there's no boot.bin.uboot: not touching it" ;; esac
		fi
		newboot="$tool/boot.bin"
		what=$("$tool/m1n1boot" -base "$base" -kernel "$IMAGE" -o "$newboot") || die "couldn't build the new m1n1/boot.bin"
		say "  $what"
		confirm "Make m1n1 boot godeep directly (U-Boot's m1n1/boot.bin is kept as boot.bin.uboot)?"
	elif [ "$AUTOBOOT" = 1 ]; then
		confirm "Copy godeep.img to the godeep boot partition and make U-Boot boot it automatically?"
	else
		confirm "Copy godeep.img to the godeep boot partition?"
	fi
	[ -f "$mnt/godeep.img" ] && run $S cp "$mnt/godeep.img" "$mnt/godeep.img.prev"
	run $S cp "$IMAGE" "$mnt/godeep.img"
	ok "godeep.img is on the godeep boot partition (the previous one, if any, is godeep.img.prev)"

	if [ "$DIRECT" = 1 ]; then
		[ -f "$saved" ] || run $S cp "$boot" "$saved"
		run $S cp "$newboot" "$boot"
		ok "m1n1/boot.bin: m1n1 → godeep (U-Boot's is m1n1/boot.bin.uboot; --uboot puts it back)"
	elif [ -f "$saved" ]; then
		run $S cp "$saved" "$boot"
		run $S rm "$saved"
		ok "m1n1/boot.bin: U-Boot restored"
	fi
	[ -n "$tool" ] && rm -rf "$tool"

	# The fallback, always written: boot by hand from U-Boot's prompt (--uboot / --no-autoboot).
	put "$mnt/GODEEP-BOOT.txt" <<TXT
To use U-Boot instead of m1n1 → godeep: tools/godeep-partition.sh install --no-autoboot
Then boot godeep by hand: press a key during U-Boot's countdown, then type:

load nvme 0:$part \${kernel_addr_r} godeep.img
booti \${kernel_addr_r} - \${fdtcontroladdr}

If nvme 0:$part is wrong, "part list nvme 0" lists the partitions (the godeep
one is the 500 MB EFI partition between the macOS container and GODEEPDATA).
TXT

	if [ "$DIRECT" = 1 ]; then
		: # U-Boot doesn't run; its extlinux.conf is left as it is
	elif [ "$AUTOBOOT" = 1 ]; then
		# U-Boot's standard boot scans the boot partition for
		# extlinux/extlinux.conf and boots arm64 Images with booti. With no
		# FDT line it passes the device tree it got from m1n1 (framebuffer
		# included). Ours is marked "# godeep" so it can be updated safely.
		if [ -f "$mnt/extlinux/extlinux.conf" ] && ! grep -q '^# godeep' "$mnt/extlinux/extlinux.conf"; then
			warn "an extlinux.conf that isn't godeep's exists; left alone (boot by hand, GODEEP-BOOT.txt)"
		else
			$S mkdir -p "$mnt/extlinux"
			put "$mnt/extlinux/extlinux.conf" <<CONF
# godeep (written by tools/godeep-partition.sh)
# U-Boot boots this automatically from the godeep boot entry.
# Delete this file to make U-Boot stop at its prompt instead.
TIMEOUT 30
DEFAULT godeep
LABEL godeep
    MENU LABEL godeep (Go kernel)
    KERNEL /godeep.img
CONF
			ok "autoboot: extlinux/extlinux.conf"
		fi
	elif [ -f "$mnt/extlinux/extlinux.conf" ] && grep -q '^# godeep' "$mnt/extlinux/extlinux.conf"; then
		run $S rm "$mnt/extlinux/extlinux.conf"
		ok "autoboot off: removed godeep's extlinux.conf"
	fi
	sync
	[ -n "$was_mounted" ] || run sudo diskutil unmount "$esp"
}

boot_help() {
	local part=${1##*s}
	say ""
	ok "Done. To boot godeep:"
	say "  1. Shut down. Press and hold the power button until \"Loading startup options\" appears."
	if [ "$DIRECT" = 1 ]; then
		say "  2. Choose ${bold}godeep${off}. m1n1 boots godeep directly: no U-Boot, no countdown."
	elif [ "$AUTOBOOT" = 1 ]; then
		say "  2. Choose ${bold}godeep${off}. m1n1 starts U-Boot, which boots godeep after its countdown."
	else
		say "  2. Choose ${bold}godeep${off}. m1n1 starts U-Boot; boot godeep by hand (below)."
	fi
	say "  3. Expected: the screen clears, the godeep banner and a \"board:\" line appear, then"
	say "     lsdev, ps, mem and uptime run by themselves (there's no keyboard driver yet)."
	say ""
	if [ "$DIRECT" = 1 ]; then
		say "  If U-Boot's text or Omarchy appears, you didn't boot the godeep entry: m1n1 on it no"
		say "  longer starts U-Boot. If the screen stays on the Apple logo or m1n1's output, godeep"
		say "  stopped before its console came up (docs/TargetMyMac.md §6.8)."
	else
		say "  By hand: press a key during U-Boot's countdown, then type (also on the boot"
		say "  partition as GODEEP-BOOT.txt):"
		say "      ${bold}load nvme 0:$part \${kernel_addr_r} godeep.img${off}"
		say "      ${bold}booti \${kernel_addr_r} - \${fdtcontroladdr}${off}"
		say "  ($part is right only after a reboot: macOS numbers new partitions differently until then.)"
		say ""
		say "  If the screen stays on U-Boot's output, godeep stopped before its console came up"
		say "  (docs/TargetMyMac.md §6.8)."
	fi
	say "  This is godeep's first run on real hardware: untested there."
	say "  Hold the power button any time to get back to macOS."
	say "  After changing godeep, rerun ${bold}tools/godeep-partition.sh install${off} to update it."
}

# ---------- undo ----------

undo() {
	local c esp data_id
	c=$(godeep_container || true); esp=$(godeep_esp || true)
	data_id=$(layout | awk '/GODEEPDATA/ { print $NF }')
	[ -n "$c$esp$data_id" ] || { ok "nothing of godeep's to remove"; exit 0; }
	step "Remove godeep"
	say "  godeep stub container: ${c:-none}   godeep ESP: ${esp:-none}   GODEEPDATA: ${data_id:-none}"
	[ -n "$c" ] && [ "$c" = "$(diskutil apfs list | awk '/APFS Container Reference:/ {c=$NF} /Name: +Omarchy \(/ {print c; exit}')" ] &&
		die "refusing: $c is Omarchy's container"
	confirm "Delete these godeep partitions and give the space back to Macintosh HD?"
	[ -n "$c" ] && run sudo diskutil apfs deleteContainer "$c"
	[ -n "$esp" ] && run sudo diskutil eraseVolume free none "$esp"
	[ -n "$data_id" ] && run sudo diskutil eraseVolume free none "$data_id"
	run sudo diskutil apfs resizeContainer "$CONTAINER" 0
	show_status
}

case "$CMD" in
setup) setup ;;
finish) finish ;;
install)
	esp=$(godeep_esp || true)
	[ -n "$esp" ] || die "no godeep boot partition (EFI - GODEE…) yet: run tools/godeep-partition.sh first"
	install_image "$esp"
	boot_help "$esp"
	;;
status) show_status ;;
undo) undo ;;
esac
