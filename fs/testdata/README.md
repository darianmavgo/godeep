# Filesystem test images

Made by `tools/mktestfs.sh` with the real tools (e2fsprogs, dosfstools,
mtools), from this tree:

| Path | Content |
|---|---|
| `hello.txt` | `hello from godeep\n` |
| `docs/readme.md` | `# docs\nread-only for now.\n` |
| `A Long File Name.txt` | `long names work\n` |
| `deep/a/b/c.txt` | `deep\n` |
| `empty.txt`, `empty-dir/` | empty |
| `big.bin` | 200000 bytes, byte i = (i*7 + i/251) mod 256 |
| `many/f001.txt` … `many/f300.txt` | `file NNN\n` |
| `link-to-hello` → `hello.txt`, `docs-link` → `docs/readme.md` | symlinks, ext images only |

| Image | Format | Notes |
|---|---|---|
| `fat12.img.gz` | FAT12, 2 MB, label GODEEPFAT | |
| `fat16.img.gz` | FAT16, 16 MB, label GODEEPFAT | |
| `fat32.img.gz` | FAT32, 40 MB, label GODEEPFAT | |
| `ext4.img.gz` | ext4, 4 KB blocks, label godeep-ext4 | extents, 64bit, metadata_csum; `many/` is hashed (htree) |
| `ext2.img.gz` | ext2, 1 KB blocks, label godeep-ext2 | block maps: `big.bin` needs indirect blocks |
