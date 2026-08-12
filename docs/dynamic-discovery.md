# Dynamic backend discovery

Since v0.4.0 the balancer can keep its backend list in sync with the
cluster automatically: with `--discovery-kubeconfig` set, it
periodically reads the `default/kubernetes` Endpoints object — the list
of ready apiservers, maintained by the apiservers themselves — and
reconciles the pool. Adding or removing a control plane node no longer
requires touching every worker's configuration.

## How it works

- Every `--discovery-interval` (default `30s`) the balancer GETs
  `/api/v1/namespaces/default/endpoints/kubernetes` from one of its
  **current backends** (healthy ones first). The kubeconfig's `server`
  field is deliberately ignored: requests never depend on an external
  endpoint and can never loop back through the balancer itself.
- The classic Endpoints resource is used instead of EndpointSlice on
  purpose: the Node authorizer allows kubelet credentials to read it,
  so both a dedicated ServiceAccount and the node's own kubelet.conf
  work as the credentials source.
- Discovered backends are added, vanished backends are removed and
  their in-flight connections drained — the same behavior as a SIGHUP
  reload.

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
4. **A missing kubeconfig is not an error.** Point it at
   `kubelet.conf` on a not-yet-joined worker and discovery stays
   dormant until the file appears after `kubeadm join`.

## Option A — dedicated ServiceAccount (recommended)

Works with the hardened systemd unit as shipped. One-time cluster
setup:

```console
$ kubectl apply -f deploy/discovery-rbac.yaml
```

Mint the kubeconfig (run anywhere with cluster-admin):

```console
$ TOKEN=$(kubectl -n kube-system get secret apiserver-lb-discovery-token \
    -o jsonpath='{.data.token}' | base64 -d)
$ CA=$(kubectl -n kube-system get secret apiserver-lb-discovery-token \
    -o jsonpath='{.data.ca\.crt}')
$ cat > discovery.kubeconfig << EOF
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${CA}
    server: https://unused.invalid:6443
  name: discovery
contexts:
- context: {cluster: discovery, user: discovery}
  name: discovery
current-context: discovery
users:
- name: discovery
  user:
    token: ${TOKEN}
EOF
```

(The `server` field is required by the format but never contacted.)

Install on each worker and enable:

```console
w1$ sudo mkdir -p /etc/apiserver-lb
w1$ sudo cp discovery.kubeconfig /etc/apiserver-lb/discovery.kubeconfig
w1$ sudo chmod 0644 /etc/apiserver-lb/discovery.kubeconfig
```

Mode `0644` is a deliberate trade-off: the service runs as a dynamic
non-root user and must be able to read the file, and the token inside
can only `get` the apiserver address list (see the Role). If even that
is unacceptable, use `LoadCredential=` in a unit drop-in instead.

Add to `/etc/default/apiserver-lb`:

```bash
APISERVER_LB_OPTS="--servers=... --metrics-listen=127.0.0.1:9299 \
  --discovery-kubeconfig=/etc/apiserver-lb/discovery.kubeconfig"
```

```console
w1$ sudo systemctl restart apiserver-lb
w1$ sudo journalctl -u apiserver-lb | grep discovery
# level=INFO msg="dynamic backend discovery enabled" ...
# level=INFO msg="discovery activated" ...
```

## Option B — reuse the node's kubelet credentials

`--discovery-kubeconfig=/etc/kubernetes/kubelet.conf` works because the
Node authorizer allows kubelets to read `endpoints`, and the balancer
re-reads the referenced client certificate on every request, so kubelet
certificate rotation is picked up automatically. No cluster-side setup
at all.

The catch is on the node side: `kubelet.conf` references
`/var/lib/kubelet/pki/kubelet-client-current.pem`, which is root-only
(`0600`). The hardened unit's `DynamicUser=yes` cannot read it. To use
this option you must weaken the unit in a drop-in, e.g.:

```ini
# /etc/systemd/system/apiserver-lb.service.d/10-kubelet-creds.conf
[Service]
DynamicUser=no
User=root
ReadOnlyPaths=/etc/kubernetes /var/lib/kubelet/pki
```

Weigh this consciously: the balancer then runs as root and holds the
node's API identity. Option A keeps the sandbox intact and is the
better default.

## Verifying and testing failover of the list itself

```console
# Watch a control plane addition propagate (default: within 30s):
w1$ sudo journalctl -u apiserver-lb -f | grep discovery
# msg="discovery updated backend servers" servers="[...]"
# msg="backend added by discovery" backend=10.0.0.14:6443

# Metrics reflect the new backend immediately after:
w1$ curl -sS http://127.0.0.1:9299/metrics | grep backend_healthy
```

Remember the interaction with `kubeadm`: a control plane removed with
`kubeadm reset` disappears from the Endpoints object automatically; one
that crashes hard stays listed until its apiserver lease expires
(seconds), then vanishes. Either way the health checker has usually
already drained it before discovery catches up — discovery keeps the
*list* honest, the health checker keeps the *traffic* honest.

## Troubleshooting

- `discovery is waiting for a usable kubeconfig` — the file is missing
  or unparsable; fine before `kubeadm join`, otherwise check the path.
- `discovery fetch failed` (debug level) — no backend answered; check
  RBAC (`kubectl auth can-i get endpoints/kubernetes -n default --as
  system:serviceaccount:kube-system:apiserver-lb-discovery`).
- `discovered server list rejected` — the list failed validation; the
  log line includes the reason. The current backends stay in place.
- Status 401/403 from fetch — token expired or RBAC missing; recreate
  the Secret and kubeconfig.
