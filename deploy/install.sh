#!/usr/bin/env bash
# Install local-apiserver-lb on a worker node.
#
# Usage: sudo ./install.sh [path-to-binary]
#
# The binary defaults to ./local-apiserver-lb next to this script (or the
# first argument). The script installs the systemd unit, the kubelet
# drop-in, and the environment file template (never overwriting an
# existing /etc/default/apiserver-lb), then reloads systemd. It does NOT
# start or enable the service: review /etc/default/apiserver-lb first.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="${1:-${SCRIPT_DIR}/local-apiserver-lb}"

if [[ ${EUID} -ne 0 ]]; then
    echo "error: this script must run as root (try sudo)" >&2
    exit 1
fi
if [[ ! -x "${BINARY}" ]]; then
    echo "error: binary not found or not executable: ${BINARY}" >&2
    echo "build it with 'make build' and pass its path as the first argument" >&2
    exit 1
fi

echo ">> installing binary to /usr/local/bin/local-apiserver-lb"
install -m 0755 "${BINARY}" /usr/local/bin/local-apiserver-lb

echo ">> installing systemd unit"
install -m 0644 "${SCRIPT_DIR}/apiserver-lb.service" /etc/systemd/system/apiserver-lb.service

echo ">> installing kubelet drop-in"
install -d -m 0755 /etc/systemd/system/kubelet.service.d
install -m 0644 "${SCRIPT_DIR}/kubelet.service.d/20-apiserver-lb.conf" \
    /etc/systemd/system/kubelet.service.d/20-apiserver-lb.conf

if [[ -e /etc/default/apiserver-lb ]]; then
    echo ">> /etc/default/apiserver-lb already exists; leaving it untouched"
else
    echo ">> installing environment file template to /etc/default/apiserver-lb"
    install -m 0644 "${SCRIPT_DIR}/apiserver-lb.env" /etc/default/apiserver-lb
fi

systemctl daemon-reload

cat << 'EOF'

Installation complete. Next steps:

  1. Edit /etc/default/apiserver-lb and set --servers to your control
     plane addresses (never loopback).
  2. Start and enable the service:
       systemctl enable --now apiserver-lb
  3. Verify:
       systemctl status apiserver-lb
       curl -k https://127.0.0.1:6443/version   # expect an answer from an apiserver

Do NOT point kubelet at 127.0.0.1:6443 until the service is verified.
See docs/migration.md for the full migration procedure.
EOF
