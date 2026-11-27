#!/bin/bash
# godeep-esp.sh: mount godeep's boot partition (EFI - GODEE…) without a
# password, owned by you, so `make native` / godeep-partition.sh install
# copy onto it without sudo.
#
#   tools/godeep-esp.sh setup      once: installs the helper + sudoers rule (asks for your password)
#   tools/godeep-esp.sh mount      mount it at /Volumes/GODEEP-ESP (no password)
#   tools/godeep-esp.sh unmount    unmount it (no password)
#   tools/godeep-esp.sh status     what's installed, and where it's mounted
#   tools/godeep-esp.sh uninstall  remove the helper and the sudoers rule
#
# GODEEPDATA doesn't need any of this: macOS mounts it for you at login.
# EFI partitions are never auto-mounted and need root, hence the helper.
#
# What gets installed (both root-owned, so only root can change them):
#   /usr/local/libexec/godeep-esp   mounts/unmounts exactly one partition,
#                                   identified by its GPT partition UUID
#   /etc/sudoers.d/godeep-esp       lets you (only) run "godeep-esp mount"
#                                   and "godeep-esp unmount" with no password
set -euo pipefail

HELPER=/usr/local/libexec/godeep-esp
RULE=/etc/sudoers.d/godeep-esp
DISK=${DISK:-disk0}

die() { echo "godeep-esp: $*" >&2; exit 1; }

esp_id() { diskutil list "$DISK" | awk '/EFI - GODEE/ { print $NF }'; }

installed() { [ -x "$HELPER" ] && [ -f "$RULE" ]; }

setup() {
	local id uuid tmp
	id=$(esp_id)
	[ -n "$id" ] || die "no 'EFI - GODEE…' partition on $DISK (run tools/godeep-partition.sh first)"
	uuid=$(diskutil info "$id" | awk -F': *' '/Disk \/ Partition UUID/ { print $2 }')
	[[ "$uuid" =~ ^[0-9A-F-]{36}$ ]] || die "can't read the partition UUID of $id"
	tmp=$(mktemp -d)
	trap "rm -rf '$tmp'" EXIT # expanded now: tmp is local, gone by exit

	# The helper runs as root: fixed PATH, nothing taken from the caller
	# but the verb, and it only ever touches the partition with this UUID.
	cat >"$tmp/helper" <<EOF
#!/bin/sh
# Installed by godeep's tools/godeep-esp.sh setup. Run via sudo (no password).
set -eu
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
UUID=$uuid
OWNER=$(id -u)
GROUP=$(id -g)
MNT=/Volumes/GODEEP-ESP

dev=\$(diskutil info "\$UUID" 2>/dev/null | awk -F': *' '/Device Node:/ { print \$2 }')
case "\$dev" in /dev/disk[0-9]*s[0-9]*) ;; *) echo "godeep-esp: partition \$UUID not found" >&2; exit 1 ;; esac
diskutil info "\$dev" | grep -q 'Partition Type: *EFI' || { echo "godeep-esp: \$dev isn't an EFI partition" >&2; exit 1; }
cur=\$(diskutil info "\$dev" | awk -F': *' '/Mount Point:/ { print \$2 }')

case "\${1:-}" in
mount)
	if [ -n "\$cur" ]; then echo "\$cur"; exit 0; fi
	mkdir -p "\$MNT"
	if ! mount_msdos -u "\$OWNER" -g "\$GROUP" -m 755 "\$dev" "\$MNT"; then
		rmdir "\$MNT" 2>/dev/null || true
		exit 1
	fi
	echo "\$MNT"
	;;
unmount)
	[ -n "\$cur" ] || exit 0
	sync
	diskutil unmount "\$dev" >/dev/null || umount "\$cur"
	[ "\$cur" != "\$MNT" ] || rmdir "\$MNT" 2>/dev/null || true
	;;
*)
	echo "usage: godeep-esp mount|unmount" >&2; exit 2 ;;
esac
EOF
	cat >"$tmp/rule" <<EOF
# godeep: mount/unmount godeep's boot partition without a password (tools/godeep-esp.sh)
$(id -un) ALL=(root) NOPASSWD: $HELPER mount, $HELPER unmount
EOF
	/usr/sbin/visudo -cf "$tmp/rule" >/dev/null || die "generated sudoers rule doesn't parse (not installed)"

	echo "Installing (one password prompt):"
	echo "  $HELPER  (partition $id, UUID $uuid, mounted owned by $(id -un))"
	echo "  $RULE"
	sudo install -d -o root -g wheel -m 755 /usr/local/libexec
	sudo install -o root -g wheel -m 755 "$tmp/helper" "$HELPER"
	sudo install -o root -g wheel -m 440 "$tmp/rule" "$RULE"
	sudo -n "$HELPER" unmount >/dev/null 2>&1 || true
	if sudo -k && sudo -n "$HELPER" mount >/dev/null; then
		echo "OK: mounted at $(sudo -n "$HELPER" mount) without a password"
		sudo -n "$HELPER" unmount
	else
		die "installed, but a password-free mount didn't work (is /etc/sudoers.d included? sudo grep includedir /etc/sudoers)"
	fi
}

uninstall() {
	[ -e "$HELPER" ] || [ -e "$RULE" ] || { echo "nothing installed"; return; }
	[ ! -x "$HELPER" ] || sudo "$HELPER" unmount || true
	sudo rm -f "$RULE" "$HELPER"
	echo "removed $RULE and $HELPER"
}

status() {
	if installed; then echo "installed: $HELPER, $RULE"; else echo "not installed (tools/godeep-esp.sh setup)"; fi
	local id
	id=$(esp_id)
	[ -n "$id" ] || { echo "no godeep boot partition on $DISK"; return; }
	diskutil info "$id" | awk -F': *' '/Device Node|Volume Name|Mount Point|Partition UUID/ { print "  " $1 ": " $2 }'
}

case "${1:-}" in
setup) setup ;;
mount)
	installed || die "not set up yet: tools/godeep-esp.sh setup"
	sudo -n "$HELPER" mount ;;
unmount | umount)
	installed || die "not set up yet: tools/godeep-esp.sh setup"
	sudo -n "$HELPER" unmount ;;
status) status ;;
uninstall) uninstall ;;
*) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
