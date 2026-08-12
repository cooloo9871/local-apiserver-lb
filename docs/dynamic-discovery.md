# Dynamic backend discovery

Since v0.4.0 the balancer can keep its backend list in sync with the
cluster automatically: with `--discovery-kubeconfig` set, it
periodically reads the `default/kubernetes` Endpoints object — the list
of ready apiservers, maintained by the apiservers themselves — and
reconciles the pool. Adding or removing a control plane node no longer
requires touching every worker's configuration.

The credentials source is the node's own kubelet identity
(`/etc/kubernetes/kubelet.conf`): the Node authorizer allows kubelets
to read `endpoints`, so no cluster-side RBAC setup is needed at all,
and the balancer re-reads the referenced client certificate on every
request, so kubelet certificate rotation is picked up automatically.

## How it works

- Every `--discovery-interval` (default `30s`) the balancer GETs
  `/api/v1/namespaces/default/endpoints/kubernetes` from one of its
  **current backends** (healthy ones first). The kubeconfig's `server`
  field is deliberately ignored: requests never depend on an external
  endpoint and can never loop back through the balancer itself.
- The classic Endpoints resource is used instead of EndpointSlice on
  purpose: it is what the Node authorizer permits kubelet credentials
  to read.
- Discovered backends are added; vanished backends are removed and
  their in-flight connections drained.

Safety guards, in order:

1. **The static `--servers` list remains the bootstrap seed.**
   Discovery only adjusts the pool after the balancer is already
   serving; if discovery never succeeds, the balancer keeps working
   with the static list.
2. **An empty endpoint list is never applied.** A transiently empty
   object cannot wipe the pool.
3. **Every discovered list passes the same validation as `--servers`**
   (loopback rejection, listen-address self-forwarding, syntax). A
   rejected list is logged and ignored.
4. **A missing kubeconfig is not an error.** On a not-yet-joined
   worker, `kubelet.conf` does not exist; discovery stays dormant and
   activates automatically once `kubeadm join` creates it.

## Setup

`kubelet.conf` and the client certificate it references
(`/var/lib/kubelet/pki/kubelet-client-current.pem`) are root-only, so
the service must run as root with read-only access to those paths — a
deliberate trade-off: the balancer holds the node's API identity in
exchange for zero cluster-side setup. Install the unit drop-in
alongside the service (before or after join, either works):

```console
$ sudo mkdir -p /etc/systemd/system/apiserver-lb.service.d
$ sudo tee /etc/systemd/system/apiserver-lb.service.d/10-kubelet-creds.conf << 'EOF'
[Service]
DynamicUser=no
User=root
ReadOnlyPaths=/etc/kubernetes /var/lib/kubelet/pki
EOF
```

Add the flags to `/etc/default/apiserver-lb` (see below for
`--state-file`):

```bash
APISERVER_LB_OPTS="--servers=... --metrics-listen=127.0.0.1:9299 \
  --discovery-kubeconfig=/etc/kubernetes/kubelet.conf \
  --state-file=/var/lib/apiserver-lb/servers.json"
```

```console
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now apiserver-lb   # or restart if already running
$ sudo journalctl -u apiserver-lb | grep discovery
```

Expected log progression:

- before `kubeadm join`: `discovery is waiting for a usable kubeconfig`
  — normal, the file does not exist yet
- after join (within one interval): `discovery activated`
- on every control plane change: `discovery updated backend servers`,
  followed by `backend added by discovery` / `backend removed by
  discovery` lines

## Surviving restarts: `--state-file`

Discovery updates only the in-memory pool; the static `--servers` seed
in your configuration never changes. After a restart the balancer
starts from that seed again — fine while at least one seed address is
still a control plane, but if the entire control plane set has been
replaced over time, a restart would boot from all-dead seeds and
discovery could never bootstrap ("restart amnesia").

`--state-file` closes that gap, the same way K3s/RKE2 persist their
balancer state:

- Every applied discovery update is written atomically to the file.
- At startup, a valid state file **supersedes** `--servers` (logged as
  `restored backend servers from state file`); a missing or invalid one
  falls back to the seed. The same validation as `--servers` applies.
- The shipped unit sets `StateDirectory=apiserver-lb`, so
  `/var/lib/apiserver-lb` exists and is writable.
- Requires discovery: without it the list never changes and the flag is
  rejected at startup.

With the state file enabled, refreshing the seed list after control
plane turnover becomes cosmetic rather than required — though keeping
it roughly current is still good hygiene for the day the state file is
lost.

## Verifying a control plane change end to end

```console
# Watch a control plane addition propagate (default: within 30s):
w1$ sudo journalctl -u apiserver-lb -f | grep discovery
# msg="discovery updated backend servers" servers="[...]"
# msg="backend added by discovery" backend=10.0.0.14:6443

# Metrics and the state file reflect it immediately after:
w1$ curl -sS http://127.0.0.1:9299/metrics | grep backend_healthy
w1$ sudo cat /var/lib/apiserver-lb/servers.json
```

Remember the interaction with `kubeadm`: a control plane removed with
`kubeadm reset` disappears from the Endpoints object automatically; one
that crashes hard stays listed until its apiserver lease expires
(seconds), then vanishes. Either way the health checker has usually
already drained it before discovery catches up — discovery keeps the
*list* honest, the health checker keeps the *traffic* honest.

## Troubleshooting

- `discovery is waiting for a usable kubeconfig` — fine before
  `kubeadm join`. After join it means the file is unreadable: check
  that the `10-kubelet-creds.conf` drop-in is installed and was applied
  (`ps -o user= -C local-apiserver-lb` must print `root`; re-run
  `systemctl daemon-reload && systemctl restart apiserver-lb` after
  adding it).
- `discovery fetch failed` (debug level) — no backend answered; check
  network reachability to the control planes.
- `discovered server list rejected` — the list failed validation; the
  log line includes the reason. The current backends stay in place.
- Status 401/403 from fetch — the kubelet client certificate is invalid
  or the node was removed from the cluster; check
  `openssl x509 -in /var/lib/kubelet/pki/kubelet-client-current.pem -noout -dates`.
