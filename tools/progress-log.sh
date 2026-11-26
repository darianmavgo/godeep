#!/bin/sh
# Append an entry to the progress log of the native-Mac work:
# docs/TargetMyMacProgress.log, then .2.log, .3.log ... (parts).
#
#   tools/progress-log.sh "one-line entry"
#   tools/progress-log.sh <<'END'        a multi-line entry from stdin
#   ...
#   END
#   tools/progress-log.sh --check [REV]  verify every part only grew since
#                                        REV (default HEAD); the pre-commit
#                                        hook runs this on the staged files
#
# Every entry starts with a timestamp line. The log is append-only: earlier
# entries are never edited (fix a mistake by appending a correction), and
# parts are never renamed or deleted. Git enforces it: tools/githooks/
# pre-commit refuses a commit that changes anything but the end of a part
# (enable once: git config core.hooksPath tools/githooks).
#
# Parts: when the next entry would take the newest part past LIMIT bytes
# (48 MiB, safely under 50 MB), a new part starts with a pointer back to
# the previous one. PROGRESS_LOG_LIMIT overrides the limit (for tests).
set -eu
DIR="$(cd "$(dirname "$0")/../docs" && pwd)"
BASE="TargetMyMacProgress"
LIMIT="${PROGRESS_LOG_LIMIT:-50331648}"

part_name() { if [ "$1" = 1 ]; then echo "$BASE.log"; else echo "$BASE.$1.log"; fi; }

# newest existing part number (1 if none yet)
newest() {
	n=1
	while [ -e "$DIR/$(part_name $((n + 1)))" ]; do n=$((n + 1)); done
	echo "$n"
}

if [ "${1:-}" = "--check" ]; then
	rev="${2:-HEAD}"
	staged="${3:-}" # "--staged": compare the index, not the working tree
	cd "$DIR/.."
	status=0
	for f in $(git ls-tree --name-only "$rev" docs/ 2>/dev/null | grep "^docs/$BASE\(\.[0-9]*\)\{0,1\}\.log$" || true); do
		old="$(mktemp)"; new="$(mktemp)"
		git show "$rev:$f" > "$old"
		if [ "$staged" = "--staged" ]; then
			git show ":$f" > "$new" 2>/dev/null || { echo "progress log: $f was deleted" >&2; status=1; continue; }
		else
			[ -e "$f" ] || { echo "progress log: $f was deleted" >&2; status=1; continue; }
			cp "$f" "$new"
		fi
		size=$(wc -c < "$old" | tr -d ' ')
		if ! head -c "$size" "$new" | cmp -s - "$old"; then
			echo "progress log: $f was edited, not appended to (append a correction instead)" >&2
			status=1
		fi
		rm -f "$old" "$new"
	done
	exit $status
fi

if [ $# -gt 0 ]; then
	body="$*"
else
	body="$(cat)"
fi
[ -n "$body" ] || { echo "progress-log: empty entry" >&2; exit 1; }

entry="$(printf '=== %s ===\n%s\n\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$body"; printf x)"
entry="${entry%x}"
n="$(newest)"
log="$DIR/$(part_name "$n")"
size=0
[ -e "$log" ] && size=$(wc -c < "$log" | tr -d ' ')
add=$(printf '%s' "$entry" | wc -c | tr -d ' ')
if [ "$size" -gt 0 ] && [ $((size + add)) -gt "$LIMIT" ]; then
	prev="$(part_name "$n")"
	n=$((n + 1))
	log="$DIR/$(part_name "$n")"
	printf '=== %s ===\nPart %d of the progress log; continues %s.\n\n' \
		"$(date '+%Y-%m-%dT%H:%M:%S%z')" "$n" "$prev" > "$log"
fi
printf '%s' "$entry" >> "$log"
echo "appended to docs/$(basename "$log")"
