#!/usr/bin/env bash
set -euo pipefail

# Install the bounded stop before doing any package downloads. A running or
# prepared job must never be interrupted by this instance-side safety timer.
cat > /usr/local/sbin/switchyard-safe-stop <<'GUARD'
#!/usr/bin/env bash
set -euo pipefail
worker=/home/ubuntu/.local/bin/switchyard
# Transfer and prepare have SSH traffic before a job appears in worker load.
# Never shut down while an SSH connection may still be sending that job.
connections=$(ss -Htn state established '( sport = :22 )') || exit 1
[[ -z "$connections" ]] || exit 0
if [ -x "$worker" ]; then
    load=$(runuser -u ubuntu -- "$worker" worker --root /home/ubuntu/.local/share/switchyard/worker load) || exit 1
    [ "$load" = 0 ] || exit 0
fi
exec /usr/sbin/shutdown -h now
GUARD
chmod 0755 /usr/local/sbin/switchyard-safe-stop
cat > /etc/systemd/system/switchyard-stop.service <<'UNIT'
[Unit]
Description=Stop unattended Switchyard worker
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/switchyard-safe-stop
UNIT
cat > /etc/systemd/system/switchyard-stop.timer <<'UNIT'
[Unit]
Description=Bound Switchyard worker uptime on every boot
[Timer]
OnBootSec=2h
OnUnitInactiveSec=15min
Unit=switchyard-stop.service
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now switchyard-stop.timer

if [[ "${1:-}" == --timer-only ]]; then
    exit 0
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates git rsync podman catatonit uidmap passt slirp4netns fuse-overlayfs iproute2
# The SSH user runs Podman directly in its own user namespace; no Docker
# socket or persistent container daemon is exposed to jobs.
# Cloud images differ in whether their default user already has subordinate
# IDs. Only add a range when needed, and never reuse an existing allocation.
python3 - <<'PY'
from pathlib import Path
from subprocess import run

size = 65_536
for kind, option in (("subuid", "--add-subuids"), ("subgid", "--add-subgids")):
    allocations = []
    owned = False
    for line in Path("/etc", kind).read_text().splitlines():
        if not line or line.startswith("#"):
            continue
        name, start, count = line.split(":")
        start, count = int(start), int(count)
        if name == "ubuntu" and count >= size:
            owned = True
        allocations.append((start, start + count))
    if owned:
        continue
    candidate = 100_000
    for start, end in sorted(allocations):
        if candidate + size <= start:
            break
        candidate = max(candidate, end)
    if candidate + size >= 2**32:
        raise SystemExit("no free subordinate ID range")
    run(["usermod", option, f"{candidate}-{candidate + size - 1}", "ubuntu"], check=True)
PY
install -d -m 0755 /var/lib/switchyard
date -u +'%FT%TZ' > /var/lib/switchyard/ready
