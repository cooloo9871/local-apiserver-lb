# local-apiserver-lb

A node-local L4 TCP load balancer for the Kubernetes apiserver, written in
Go with zero runtime dependencies. It runs as a small systemd service on
every **worker** node, listens on `127.0.0.1:6443`, and forwards each TCP
connection to a healthy control plane apiserver — with active health
checking and immediate draining of connections to failed backends.

kubelet and kube-proxy simply connect to `127.0.0.1:6443`.

This is the same pattern RKE2/K3s agents use (`Running load balancer
rke2-api-server-agent-load-balancer 127.0.0.1:6443 -> [...]`), extracted
into a standalone service for kubeadm clusters.

## Why (and the relationship to kube-vip)

In a kubeadm HA cluster, kube-vip typically provides a VIP consumed by:

1. external `kubectl` users, and
2. kubelet / kube-proxy on worker nodes.

Pods inside the cluster already reach the apiserver through the
`kubernetes` Service ClusterIP (`10.96.0.1:443`), load-balanced by
kube-proxy — they never needed the VIP.

The VIP has real costs for case 2: it is active on exactly one node, so
every worker in the cluster is affected during failover; and kube-vip
needs a static pod on the control plane, a coordination Lease, and extra
RBAC.

This project removes the worker-side dependency on the VIP:

- **Failure isolation**: each worker has its own balancer; a problem
  affects one node, not the whole cluster.
- **Fast failover**: an unhealthy apiserver is detected within
  `health-interval × fall` (6 s by default) and all connections to it are
  actively closed, so kubelet reconnects immediately instead of hanging
  on a dead TCP session for minutes.
- **No TLS termination**: pure TCP passthrough. kubelet ↔ apiserver TLS
  stays end-to-end; this process never sees plaintext.

kube-vip (or DNS, or an external LB) is still needed for external
`kubectl` and for `kubeadm join` of new control plane nodes — see
[docs/migration.md](docs/migration.md) §3.

## Architecture

```
        worker node                              control plane
┌──────────────────────────────┐
│  kubelet      kube-proxy     │           ┌─────────────────────┐
│     │             │          │      ┌───▶│ cp1 apiserver :6443 │
│     └──────┬──────┘          │      │    └─────────────────────┘
│            ▼                 │      │    ┌─────────────────────┐
│   https://127.0.0.1:6443     │      ├───▶│ cp2 apiserver :6443 │
│            │                 │      │    └─────────────────────┘
│  ┌─────────▼──────────────┐  │      │    ┌─────────────────────┐
│  │   local-apiserver-lb   │──┼──────┴───▶│ cp3 apiserver :6443 │
│  │  (systemd, TCP proxy)  │  │  round-   └─────────────────────┘
│  └────────────────────────┘  │  robin /            ▲
│      health checks ──────────┼──least-conn─────────┘
└──────────────────────────────┘  GET /readyz every 3s
```

- One connection in, one connection out; both directions are copied
  byte-for-byte with correct TCP half-close handling.
- A backend that fails `fall` consecutive probes is marked unhealthy,
  **all its in-flight connections are force-closed**, and new connections
  go elsewhere. After `rise` consecutive successes it is added back
  (existing connections are not rebalanced).
- If a dial fails, the next candidate is tried transparently; the client
  never sees the error unless every backend is unreachable.
- If **all** backends are unhealthy, the balancer degrades to best-effort:
  each connection tries every backend in order, and a warning is logged.

## Quick start

### Option 1: download a release binary (no Go required)

Pre-built static binaries (`linux/amd64`, `linux/arm64`; no glibc
dependency) are published on the
[releases page](https://github.com/cooloo9871/local-apiserver-lb/releases).
On the worker node:

```console
$ curl -sLO https://github.com/cooloo9871/local-apiserver-lb/releases/latest/download/local-apiserver-lb-linux-amd64
$ curl -sLO https://github.com/cooloo9871/local-apiserver-lb/releases/latest/download/SHA256SUMS
$ sha256sum -c SHA256SUMS --ignore-missing
local-apiserver-lb-linux-amd64: OK
$ sudo install -m 0755 local-apiserver-lb-linux-amd64 /usr/local/bin/local-apiserver-lb
```

(Pin a specific version by replacing `latest/download` with
`download/v0.1.0`.)

The systemd unit and configuration templates live in this repository's
`deploy/` directory — fetch them alongside the binary:

```console
$ base=https://raw.githubusercontent.com/cooloo9871/local-apiserver-lb/main/deploy
$ sudo curl -sL -o /etc/systemd/system/apiserver-lb.service $base/apiserver-lb.service
$ sudo curl -sL -o /etc/default/apiserver-lb $base/apiserver-lb.env
$ sudo mkdir -p /etc/systemd/system/kubelet.service.d
$ sudo curl -sL -o /etc/systemd/system/kubelet.service.d/20-apiserver-lb.conf \
    $base/kubelet.service.d/20-apiserver-lb.conf
$ sudo systemctl daemon-reload
```

Edit `/etc/default/apiserver-lb` and set `--servers` to your control
plane addresses, then enable and start the service (`enable --now` also
makes it start on boot):

```console
$ sudo vi /etc/default/apiserver-lb
$ sudo systemctl enable --now apiserver-lb
```

Quick smoke test — the full pre-cutover checklist lives in
[docs/migration.md](docs/migration.md) (Phase 4, step 2); run that
before switching kubelet:

```console
$ curl -sSk https://127.0.0.1:6443/version
# apiserver version JSON. On clusters with --anonymous-auth=false this
# returns a 401 Status object instead — either way the response comes
# from a real apiserver, which proves proxying works.
```

### Option 2: build from source

Requirements: Go ≥ 1.22 to build; systemd on the target node.

```console
$ make build          # static binary in bin/ (CGO_ENABLED=0)
$ make build-all      # linux/amd64 + linux/arm64 in dist/
$ make test           # go test -race ./...
```

On a worker node:

```console
$ sudo ./deploy/install.sh bin/local-apiserver-lb
$ sudo vi /etc/default/apiserver-lb    # set --servers to your CP addresses
$ sudo systemctl enable --now apiserver-lb
$ curl -sSk https://127.0.0.1:6443/version   # answered by a real apiserver
```

Or run it directly:

```console
$ local-apiserver-lb \
    --servers 10.0.0.11:6443,10.0.0.12:6443,10.0.0.13:6443 \
    --metrics-listen 127.0.0.1:9299
```

Whichever option you choose: starting the service has no effect on the
cluster by itself — kubelet keeps using its current endpoint until you
deliberately switch it. Do **not** point kubelet at `127.0.0.1:6443`
until you have followed [docs/migration.md](docs/migration.md).

## Configuration

Flags take precedence over the optional YAML config file (`--config`).

| Flag | Default | Description |
|---|---|---|
| `--listen` | `127.0.0.1:6443` | Listen address. Non-loopback addresses are **refused** unless `--allow-non-loopback` is set, because the backends accept connections without any additional authentication layer. |
| `--servers` | *(required)* | Comma-separated backend list (`host:port`). Loopback entries — including hostnames that *resolve* to loopback via `/etc/hosts` — are rejected to prevent forwarding loops. |
| `--balance` | `round-robin` | `round-robin` or `least-conn`. apiserver watches are long-lived, so `least-conn` gives a more even distribution after node restarts. |
| `--health-check-mode` | `readyz` | `readyz` (GET `/readyz`, expect 200) or `tcp` (plain dial). Use `tcp` if your cluster disables anonymous auth (see [Troubleshooting](#readyz-returns-401)). |
| `--health-interval` | `3s` | Time between probes of one backend. |
| `--health-timeout` | `3s` | Per-probe timeout. |
| `--fall` | `2` | Consecutive failures before a backend is unhealthy. |
| `--rise` | `2` | Consecutive successes before it is healthy again. |
| `--dial-timeout` | `3s` | Backend dial timeout. |
| `--ca-file` | *(empty)* | CA bundle for health-check TLS. When set, the certificate **chain** is verified but the **hostname is not** — backends are dialed by IP, and kubeadm apiserver certs do not necessarily contain every control plane IP. Typically `/etc/kubernetes/pki/ca.crt`. |
| `--insecure-skip-verify` | `true` | Skip TLS verification on health checks (the probe only proves liveness). Automatically disabled when `--ca-file` is set; combining `--ca-file` with an explicit `true` is an error. |
| `--keepalive-period` | `30s` | TCP keepalive on both legs; `0` disables. |
| `--metrics-listen` | *(empty)* | Metrics/health HTTP address (e.g. `127.0.0.1:9299`); empty disables. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. Per-connection events are logged only at `debug`. |
| `--log-format` | `text` | `text` or `json` (to stderr, for journald). |
| `--shutdown-grace` | `10s` | Grace period for in-flight connections on SIGTERM/SIGINT. |
| `--allow-non-loopback` | `false` | Allow binding to a non-loopback address. Understand the exposure before using this. |
| `--config` | *(empty)* | Optional YAML config file. |
| `--discovery-kubeconfig` | *(empty)* | Enable [dynamic backend discovery](docs/dynamic-discovery.md): credentials for reading the `default/kubernetes` Endpoints object. The file may not exist yet at startup (e.g. a pre-join `kubelet.conf`); discovery waits for it. |
| `--discovery-interval` | `30s` | How often to refresh the backend list when discovery is enabled. |
| `--version` | | Print version, commit, and build date, then exit. |

Configuration is validated at startup and the process refuses to run on
any error (empty server list, malformed `host:port`, loopback loops, a
server entry equal to the listen address, unreadable `--ca-file`,
contradictory TLS flags, unresolvable hostnames).

### YAML config file

The config file is a deliberately restricted YAML subset: flat
`key: value` scalars and string lists (block or flow style), `#`
comments, optional quotes. Nested mappings, anchors, and multi-line
strings are not supported. Keys are the flag names:

```yaml
# /etc/apiserver-lb/config.yaml
listen: 127.0.0.1:6443
balance: least-conn
servers:
  - 10.0.0.11:6443
  - 10.0.0.12:6443
  - 10.0.0.13:6443
```

### Reloading the server list (SIGHUP)

When running with `--config`, `SIGHUP` re-reads **only** the `servers`
list — you can add or remove control planes without restarting (and
without dropping connections to retained backends):

```console
$ sudo vi /etc/apiserver-lb/config.yaml
$ sudo systemctl kill -s HUP apiserver-lb
```

The new list is validated first; a bad list is rejected and the current
backends stay in place. Removed backends have their in-flight connections
drained. Changes to any other key are ignored until restart. Without
`--config`, SIGHUP logs a warning and does nothing.

### Dynamic backend discovery

With `--discovery-kubeconfig` the balancer keeps the backend list in
sync with the cluster automatically, RKE2-style: it periodically reads
the `default/kubernetes` Endpoints object (where the apiservers publish
their own addresses) through its current backends, and reconciles the
pool — added control planes start receiving traffic, removed ones are
drained. The static `--servers` list remains the bootstrap seed and
fallback; empty or invalid discovered lists are never applied.

Credentials are either a dedicated ServiceAccount that can only `get`
that one object (`deploy/discovery-rbac.yaml`, recommended) or the
node's own `kubelet.conf`. Setup, trade-offs, and troubleshooting:
[docs/dynamic-discovery.md](docs/dynamic-discovery.md).

## Metrics

### Choosing the metrics port

Pick a localhost port nothing else on the node uses. Because this
service starts **before** kubelet (and therefore before any CNI
daemonset), it always wins the bind — the colliding CNI pod is the one
that crash-loops with `bind: address already in use`, which is easy to
misdiagnose as a CNI problem.

The examples use `9299`, chosen against the default host-bound TCP
ports of the common CNIs (verified against upstream docs, 2026-08):

| CNI | Default host-bound TCP ports |
|---|---|
| **Calico** | `179` (BGP), `5473` (Typha), `9098` (Typha health), **`9099` (Felix health)**, `9091` (Felix/Typha Prometheus metrics, off by default) |
| **Canal** | the Calico Felix ports above + Flannel below |
| **Flannel** | none — VXLAN is UDP `8472`, healthz is disabled by default (`--healthz-port=0`) |
| **Cilium** | `4240` (health), `4244`/`4245` (Hubble), `4250`/`4251` (auth/SPIRE), `6060`–`6062` (pprof), `9878`/`9879` (envoy/agent health), `9890`–`9893` (gops), `9901` (envoy admin), `9962`–`9965` (Prometheus metrics) |

`9299` collides with none of them. Also avoid `9100` (node-exporter),
`9153` (CoreDNS), and `10248`–`10260` (kubelet/kube-proxy). Whatever
you pick, check the node first:

```console
$ ss -tlnp | grep 9299    # must be empty before you enable the service
```

With `--metrics-listen 127.0.0.1:9299`:

| Endpoint | Meaning |
|---|---|
| `/healthz` | The balancer process itself is alive. |
| `/readyz` | 200 if at least one backend is healthy, else 503. |
| `/metrics` | Prometheus text format. |

Exported metrics, all labeled `{server="<backend>"}`:

| Metric | Type | Meaning |
|---|---|---|
| `apiserver_lb_backend_healthy` | gauge | 1 healthy / 0 unhealthy |
| `apiserver_lb_backend_connections` | gauge | in-flight proxied connections |
| `apiserver_lb_connections_total` | counter | connections proxied |
| `apiserver_lb_dial_errors_total` | counter | failed dials |
| `apiserver_lb_health_check_failures_total` | counter | failed probes |
| `apiserver_lb_connections_drained_total` | counter | connections force-closed by draining |

## Migrating from kube-vip

The complete, step-by-step procedure — including the certificate SAN
trap, the shared kube-proxy ConfigMap, the `cluster-info` update needed
before removing kube-vip, canary order, rollback, and a failover drill —
is in **[docs/migration.md](docs/migration.md)**. Read it in full before
touching a production cluster; every section exists because skipping it
breaks clusters in practice.

The short version:

1. **Pick a path for the apiserver certificate** (docs §2):
   - *Path A (no cert changes)*: keep using the existing
     `controlPlaneEndpoint` DNS name and point it at `127.0.0.1` in each
     worker's `/etc/hosts`. kubelet.conf and kube-proxy stay untouched.
   - *Path B (re-sign certs)*: add `127.0.0.1` and `localhost` to
     `apiServer.certSANs` in the `kubeadm-config` ConfigMap, re-sign on
     all three control planes, then switch kubeconfigs to
     `https://127.0.0.1:6443`. **Must** be in the ConfigMap, not only a
     one-time `--apiserver-cert-extra-sans`, or the next
     `kubeadm certs renew` silently drops the SAN and every worker fails
     at once.
2. **Install and verify the LB on one worker** (docs §4), switch that
   worker, `kubectl wait --for=condition=Ready node/<name>`, then proceed
   node by node. Never switch all workers in parallel.
3. **Update `kube-public/cluster-info` and plan the external endpoint**
   (docs §3, §5) *before* removing kube-vip — future `kubeadm join` reads
   the apiserver address from `cluster-info`, not from your command line.
4. **Run the failover drill** (docs §7) before trusting it in production.
5. Only after every worker is migrated and verified: remove the kube-vip
   static pod and its Lease/RBAC leftovers (docs §6).

## Troubleshooting

### x509: certificate is valid for ..., not 127.0.0.1

kubelet (or kubectl) is connecting to `127.0.0.1:6443` but the apiserver
certificate does not contain `127.0.0.1`. kubeadm's default SANs are the
node name, `kubernetes`, `kubernetes.default`,
`kubernetes.default.svc`, `kubernetes.default.svc.<dnsDomain>`, the
first Service CIDR IP, the advertise address, and the
`controlPlaneEndpoint` — **not** `127.0.0.1`.

- If you chose migration Path A: this error means something switched a
  kubeconfig to `127.0.0.1` directly. Use the `controlPlaneEndpoint` name
  + `/etc/hosts` instead, or move to Path B.
- If you chose Path B: verify the SAN is really present —
  `openssl s_client -connect <cp>:6443 </dev/null 2>/dev/null | openssl x509 -noout -text | grep -A1 'Subject Alternative Name'`.
- If it **was** present and disappeared after a `kubeadm certs renew` or
  upgrade: the SAN was signed with a one-time flag and never written to
  the `kubeadm-config` ConfigMap. Fix the ConfigMap and re-sign
  (docs/migration.md §2, Path B warning).

### /readyz returns 401

`/readyz` is anonymously accessible by default because the
`system:public-info-viewer` ClusterRole grants `get` on `/healthz`,
`/livez`, `/readyz`, and `/version`, and its ClusterRoleBinding includes
both `system:authenticated` and `system:unauthenticated`. If your cluster
runs the apiserver with `--anonymous-auth=false`, those requests are
rejected with 401 and every backend looks permanently unhealthy.

Options:

- Switch to `--health-check-mode tcp` (weaker: only proves the port
  accepts connections).
- On Kubernetes 1.32+, keep anonymous auth disabled globally but allow
  it *only* for the health endpoints via an `AuthenticationConfiguration`
  file (`--authentication-config`):

  ```yaml
  apiVersion: apiserver.config.k8s.io/v1
  kind: AuthenticationConfiguration
  anonymous:
    enabled: true
    conditions:
      - path: /healthz
      - path: /livez
      - path: /readyz
  ```

### kubelet cannot connect to 127.0.0.1:6443

In order:

1. `systemctl status apiserver-lb` — is the service running? If it
   restarts in a loop, `journalctl -u apiserver-lb` shows the validation
   error; the service refuses to start on bad configuration.
2. `ss -tlnp | grep 6443` — is something else (e.g. a stray apiserver on
   a control plane node — this service is for **workers only**) holding
   the port?
3. `curl -sS http://127.0.0.1:9299/readyz` — 503 means no healthy
   backends: check network reachability to the control planes and the
   `/readyz 401` issue above.
4. Startup fails with a *loopback* error: your `--servers` contains an
   entry that resolves to `127.0.0.1` (usually via `/etc/hosts`). Use the
   real control plane IPs in `--servers`; the `/etc/hosts` trick is only
   for what *kubelet* connects to, never for the backend list.

### Abnormal connection counts

- One backend holds most connections after it was down: expected with
  `round-robin` — watches are long-lived and do not rebalance. Use
  `--balance least-conn`, or wait for natural churn; recovery never moves
  existing connections by design.
- `apiserver_lb_connections_drained_total` climbing steadily: a backend
  is flapping (repeatedly crossing the fall/rise thresholds). Check that
  apiserver's health directly and consider raising `--fall`/`--rise`.
- Connections pile up while backends are down: clients keep reconnecting
  during best-effort mode; this resolves itself once a backend recovers.

## Known limitations

- **The backend list is static by default** (flags or config file +
  SIGHUP). Enable [dynamic discovery](docs/dynamic-discovery.md) with
  `--discovery-kubeconfig` to follow control plane changes
  automatically; the static list is still required as the bootstrap
  seed and fallback.
- **The balancer is a per-node single point of failure.** The blast
  radius is one node, and systemd (`Restart=always`, `RestartSec=2s`)
  restores it within seconds.
- **Restarting the balancer drops every watch on that node.** That is
  fine for one node, but restarting it on *many* nodes simultaneously
  causes a re-list spike against the apiservers — stagger mass restarts.
- Control plane nodes must not run it: their kubelets already talk to
  the local apiserver, which owns port 6443.

## Development

```console
$ make test    # go test -race ./...
$ make lint    # gofmt + go vet
$ make fmt
```

The project uses only the Go standard library at runtime. Tests spin up
real TCP/TLS servers (`httptest`) — no mocks of the network path.
