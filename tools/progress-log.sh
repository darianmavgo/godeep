#!/bin/sh
# Append an entry to docs/TargetMyMacProgress.log, the append-only log of
# progress toward running godeep on this MacBook Pro.
#
#   tools/progress-log.sh "one-line entry"
#   tools/progress-log.sh <<'END'      (a multi-line entry from stdin)
#   ...
#   END
#
# Every entry starts with a timestamp line. Earlier entries are never edited:
# on macOS the file carries the user append-only flag (chflags uappnd), so the
# OS refuses anything but appending (editors, Write tools, truncation). To fix
# a mistake, append a correction. (Removing the flag: chflags nouappnd FILE.)
set -eu
LOG="$(dirname "$0")/../docs/TargetMyMacProgress.log"

if [ $# -gt 0 ]; then
	body="$*"
else
	body="$(cat)"
fi
[ -n "$body" ] || { echo "progress-log: empty entry" >&2; exit 1; }

new=0
[ -e "$LOG" ] || new=1
{
	printf '=== %s ===\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')"
	printf '%s\n\n' "$body"
} >> "$LOG"

if [ "$new" = 1 ] && command -v chflags >/dev/null 2>&1; then
	chflags uappnd "$LOG"
fi
echo "appended to $LOG"
