#!/bin/sh
# Stop godeep VMs running under QEMU.
#
#   tools/qemu-stop.sh          graceful: press the VM's power button (QMP
#                               system_powerdown), so godeep stops its processes
#                               and powers off; fall back to SIGTERM, then SIGKILL
#   tools/qemu-stop.sh --force  SIGKILL immediately
#
# Finds VMs via bin/qemu-*.pid, and also by command line, so it also stops
# VMs started before the Makefile wrote pid files.
cd "$(dirname "$0")/.." || exit 1

force=0
[ "$1" = "--force" ] && force=1

alive() { kill -0 "$1" 2>/dev/null; }

# wait_gone PID SECONDS: wait for PID to exit; 0 = gone
wait_gone() {
	i=0
	while alive "$1"; do
		[ "$i" -ge "$(( $2 * 10 ))" ] && return 1
		sleep 0.1
		i=$((i + 1))
	done
	return 0
}

pids=""
for f in bin/qemu-*.pid; do
	[ -f "$f" ] || continue
	pid=$(cat "$f")
	alive "$pid" && pids="$pids $pid"
done
for pid in $(pgrep -f 'qemu-system-.*godeep-.*\.elf'); do
	case " $pids " in *" $pid "*) ;; *) pids="$pids $pid" ;; esac
done

if [ -z "$pids" ]; then
	echo "no godeep QEMU running"
	rm -f bin/qemu-*.pid bin/qmp-*.sock
	exit 0
fi

for pid in $pids; do
	args=$(ps -o args= -p "$pid" 2>/dev/null)
	sock=$(printf '%s\n' "$args" | sed -n 's/.*-qmp unix:\([^, ]*\).*/\1/p')

	if [ "$force" = 0 ] && [ -n "$sock" ] && [ -S "$sock" ]; then
		echo "pid $pid: pressing the VM power button (QMP $sock)"
		(printf '{"execute":"qmp_capabilities"}\n{"execute":"system_powerdown"}\n'; sleep 1) |
			nc -U "$sock" >/dev/null 2>&1
		if wait_gone "$pid" 5; then
			echo "pid $pid: godeep powered off"
			continue
		fi
		echo "pid $pid: no response from the guest"
	fi

	if [ "$force" = 0 ]; then
		echo "pid $pid: sending SIGTERM to QEMU"
		kill -TERM "$pid" 2>/dev/null
		wait_gone "$pid" 3 && { echo "pid $pid: stopped"; continue; }
	fi

	echo "pid $pid: sending SIGKILL"
	kill -KILL "$pid" 2>/dev/null
	wait_gone "$pid" 2 && echo "pid $pid: killed" || { echo "pid $pid: could not kill" >&2; exit 1; }
done
rm -f bin/qemu-*.pid bin/qmp-*.sock
