#!/bin/sh
# cpuload.sh NAME SECONDS [MIN]: how many host cores the QEMU started with
# -name NAME used over SECONDS (CPU-time delta, not ps's decaying %cpu).
# Prints "cores N.NN", plus "parallel" when N >= MIN (default 1.5).
pid=$(pgrep -f "qemu-system.*-name $1" | head -1)
[ -n "$pid" ] || { echo "no QEMU named $1"; exit 1; }
t() { ps -o time= -p "$pid" | awk -F: '{ if (NF == 3) print $1*3600 + $2*60 + $3; else print $1*60 + $2 }'; }
a=$(t); sleep "$2"; b=$(t)
echo "$a $b $2 ${3:-1.5}" | awk '{ c = ($2 - $1) / $3; printf "cores %.2f%s\n", c, (c >= $4) ? " parallel" : "" }'
