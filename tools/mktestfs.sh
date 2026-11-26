#!/bin/bash
# mktestfs: regenerate fs/testdata/*.img.gz, the filesystem images the FAT and
# ext4 readers (and bin/disk.img for QEMU) are tested against. They are made
# by the real tools, so the readers are checked against what Linux and other
# systems write, not against a writer of our own:
#   brew install e2fsprogs dosfstools mtools
# The images are committed; rerun only to change them.
set -euo pipefail
cd "$(dirname "$0")/.."
MKE2FS=${MKE2FS:-/opt/homebrew/opt/e2fsprogs/sbin/mke2fs}
MKFAT=${MKFAT:-/opt/homebrew/opt/dosfstools/sbin/mkfs.fat}
E2FSCK=${E2FSCK:-/opt/homebrew/opt/e2fsprogs/sbin/e2fsck}
out=fs/testdata
w=$(mktemp -d)
trap 'rm -rf "$w"' EXIT

# The tree (fs/testdata/README.md lists what the tests expect).
t="$w/tree"
mkdir -p "$t/docs" "$t/deep/a/b" "$t/many" "$t/empty-dir"
printf 'hello from godeep\n' >"$t/hello.txt"
printf '# docs\nread-only for now.\n' >"$t/docs/readme.md"
printf 'long names work\n' >"$t/A Long File Name.txt"
printf 'deep\n' >"$t/deep/a/b/c.txt"
: >"$t/empty.txt"
python3 -c 'import sys; sys.stdout.buffer.write(bytes((i*7+i//251)%256 for i in range(200000)))' >"$t/big.bin"
for i in $(seq -w 1 300); do printf 'file %s\n' "$i" >"$t/many/f$i.txt"; done

# FAT: no symlinks. mcopy keeps long names.
fat() { # fat NAME MB FATBITS
	rm -f "$w/$1"
	"$MKFAT" -C -F "$3" -n GODEEPFAT "$w/$1" $(($2 * 1024)) >/dev/null
	MTOOLS_SKIP_CHECK=1 mcopy -s -i "$w/$1" "$t"/* ::/
	gzip -9 -n -c "$w/$1" >"$out/$1.gz"
}
fat fat12.img 2 12
fat fat16.img 16 16
fat fat32.img 40 32

# ext4 (extents, 64-bit, checksums, hashed directories) and ext2 (block
# maps with indirect blocks: 1 KB blocks make big.bin need them).
ln -s hello.txt "$t/link-to-hello"
ln -s docs/readme.md "$t/docs-link"
"$MKE2FS" -q -F -t ext4 -O 64bit,metadata_csum -b 4096 -L godeep-ext4 -d "$t" "$w/ext4.img" 8M
"$MKE2FS" -q -F -t ext2 -b 1024 -L godeep-ext2 -d "$t" "$w/ext2.img" 4M
# mke2fs -d doesn't index directories; e2fsck -D does, so "many" gets an
# htree (hashed directory) like a directory grown on a running system.
# (Exit status 1 means "fixed something": expected.)
"$E2FSCK" -fyD "$w/ext4.img" >/dev/null || [ $? -eq 1 ]
for i in ext4.img ext2.img; do gzip -9 -n -c "$w/$i" >"$out/$i.gz"; done
ls -l "$out"
