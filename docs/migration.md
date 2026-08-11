# Migrating workers from kube-vip to local-apiserver-lb

This is a runbook. Execute the phases **in order**; each exists because
skipping it breaks clusters in practice. Commands assume a kubeadm HA
cluster (3 control planes, N workers) where kube-vip currently provides
the VIP that workers' kubelet and kube-proxy connect to.

Terminology used below:

- **CP** — control plane node (`cp1`, `cp2`, `cp3` with IPs
  `10.0.0.11-13` in examples).
- **CPE** — the cluster's `controlPlaneEndpoint` as configured in
  kubeadm (for example `k8s-api.example.com:6443`). Check yours:

  ```console
  $ kubectl -n kube-system get cm kubeadm-config -o yaml | grep controlPlaneEndpoint
  ```

---

## Phase 0 — Inventory and prerequisites

Collect these facts before changing anything:

1. **The CPE value and whether it is a DNS name or an IP.** If it is a
   DNS name, Path A below is available (recommended). If it is the VIP
   IP itself, only Path B works.
2. **Control plane addresses** for `--servers`
   (e.g. `10.0.0.11:6443,10.0.0.12:6443,10.0.0.13:6443`).
3. **Whether anonymous auth is disabled** (`--anonymous-auth=false` on
   the apiservers). If so, plan for `--health-check-mode tcp` or the
   Kubernetes 1.32+ `AuthenticationConfiguration` carve-out (README,
   "Troubleshooting → /readyz returns 401").
4. **A working `kubectl` that does not depend on the VIP** (e.g. an
   admin kubeconfig pointing directly at one CP IP). You must not lose
   cluster access halfway through.

```console
# Direct-to-CP admin access for the duration of the migration:
$ kubectl --server https://10.0.0.11:6443 get nodes
```

---

## Phase 1 — Choose the certificate path

kubelet verifies the apiserver's TLS certificate against the name it
dials. kubeadm signs the apiserver certificate with these SANs (see
`GetAPIServerAltNames` in
`cmd/kubeadm/app/util/pkiutil/pki_helpers.go`):

- DNS: the node name, `kubernetes`, `kubernetes.default`,
  `kubernetes.default.svc`, `kubernetes.default.svc.<dnsDomain>`
- IP: the first IP of the Service CIDR (usually `10.96.0.1`) and the
  node's advertise address
- plus the CPE host, plus any user-configured `certSANs`

**`127.0.0.1` is not in that list.** So a kubeconfig pointing at
`https://127.0.0.1:6443` fails TLS verification unless you change
something. Two paths:

### Path A — keep certificates untouched (recommended)

Keep every kubeconfig pointing at the CPE name (it is already in the
SANs). On each worker, make that name resolve to loopback:

```console
# on the worker (assuming CPE is k8s-api.example.com:6443)
$ echo "127.0.0.1 k8s-api.example.com" | sudo tee -a /etc/hosts
```

- `kubelet.conf` does not change.
- The kube-proxy ConfigMap does not change.
- **Port must match**: if your CPE uses a port other than 6443 (e.g.
  `k8s-api.example.com:8443`), set `--listen 127.0.0.1:8443` so the
  existing kubeconfigs keep working verbatim.
- Never put the CPE name into `--servers` — on this node it now resolves
  to loopback, and the balancer will refuse to start with a loopback
  error (that check exists precisely for this trap). `--servers` takes
  the real CP IPs.

### Path B — add 127.0.0.1 to the apiserver certificates

Use when the CPE is an IP, or when you want kubeconfigs to literally say
`https://127.0.0.1:6443`.

1. **Persist the SANs in the cluster configuration.** This is the
   critical step:

   ```console
   $ kubectl -n kube-system edit cm kubeadm-config
   ```

   ```yaml
   apiServer:
     certSANs:
     - "127.0.0.1"
     - "localhost"
   ```

   > **Warning — the renewal trap.** If `127.0.0.1` was only ever signed
   > in with a one-time `kubeadm init --apiserver-cert-extra-sans` and is
   > *not* in the `kubeadm-config` ConfigMap, then the next
   > `kubeadm certs renew` or `kubeadm upgrade` re-reads the ConfigMap
   > and re-signs **without** it. Every worker's kubelet then fails TLS
   > verification *at the same time*. The ConfigMap is the source of
   > truth; fix it first, always.

2. **Re-sign on each CP, one at a time.** Dry-run first:

   ```console
   cp1$ sudo kubeadm certs renew apiserver --dry-run   # verify no errors
   cp1$ sudo kubeadm certs renew apiserver
   # restart the apiserver static pod to pick up the new cert:
   cp1$ sudo mv /etc/kubernetes/manifests/kube-apiserver.yaml /tmp/ && sleep 5 \
         && sudo mv /tmp/kube-apiserver.yaml /etc/kubernetes/manifests/
   ```

   Verify before moving to the next CP:

   ```console
   $ openssl s_client -connect 10.0.0.11:6443 </dev/null 2>/dev/null \
       | openssl x509 -noout -text | grep -A1 'Subject Alternative Name'
   # must now include IP Address:127.0.0.1
   ```

3. **kube-proxy caveat (shared ConfigMap).** The kubeconfig in the
   `kube-system/kube-proxy` ConfigMap is used by kube-proxy on **every**
   node, control planes included. If you point it at
   `https://127.0.0.1:6443`:

   - on workers it goes through this balancer — intended;
   - on CPs there is no balancer, so it connects to the **local**
     apiserver only. That is usually acceptable (same behavior as CP
     kubelets) but it removes cross-CP failover for CP kube-proxy: if
     the local apiserver is down, that CP's kube-proxy is blind until it
     recovers. Accept this trade-off consciously, or stay on Path A
     where the ConfigMap is untouched.

   ```console
   $ kubectl -n kube-system edit cm kube-proxy
   # kubeconfig.conf -> clusters[0].cluster.server: https://127.0.0.1:6443
   # then restart kube-proxy pods node by node alongside Phase 4
   ```

---

## Phase 2 — Plan the external endpoint (before touching kube-vip)

This balancer solves **workers only**. Two consumers still need a stable
endpoint after kube-vip is gone:

- external `kubectl` users, and
- `kubeadm join --control-plane` for future CP nodes.

Options:

| Option | Trade-off |
|---|---|
| Keep kube-vip, scoped to external access only | No infrastructure change; keeps the Lease/RBAC footprint. |
| DNS round-robin (3 A records for the CPE name → CP IPs) | Simplest. **No health checking**: a dead CP stays in rotation and clients time out until DNS is fixed; TTLs delay every change. |
| External L4 load balancer (HAProxy/keepalived pair, cloud LB) | Proper health checks; more infrastructure to run. |

Decide now; implement it in Phase 5 before kube-vip is removed.

---

## Phase 3 — Update `kube-public/cluster-info`

`kubeadm join` builds the final `kubelet.conf` from the kubeconfig
embedded in the `kube-public/cluster-info` ConfigMap — **not** from the
address you type on the `kubeadm join` command line. If it still points
at the VIP when you remove kube-vip, every node joined afterwards gets a
dead server address.

```console
$ kubectl -n kube-public get cm cluster-info -o yaml | grep server:
$ kubectl -n kube-public edit cm cluster-info
# set the server to the Phase 2 endpoint (CPE name or external LB),
# NOT to 127.0.0.1 — a joining node does not have the balancer yet
```

**Join order for new workers from now on:** install and verify
local-apiserver-lb on the node **first** (Phase 4 steps 1–3), then
`kubeadm join`, then apply the Path A `/etc/hosts` entry or Path B
kubelet.conf change and restart kubelet.

---

## Phase 4 — Migrate workers, one at a time

**Never switch workers in parallel.** Each switch makes that node's
kubelet re-establish every watch; doing this to many nodes at once
creates a re-list spike on the apiservers.

For each worker `w1`:

1. **Install:**

   ```console
   w1$ sudo ./deploy/install.sh local-apiserver-lb
   w1$ sudo vi /etc/default/apiserver-lb
   # APISERVER_LB_OPTS="--servers=10.0.0.11:6443,10.0.0.12:6443,10.0.0.13:6443 \
   #   --ca-file=/etc/kubernetes/pki/ca.crt --metrics-listen=127.0.0.1:9299"
   w1$ sudo systemctl enable --now apiserver-lb
   ```

   (Workers have `/etc/kubernetes/pki/ca.crt` from `kubeadm join`; drop
   `--ca-file` or use `--health-check-mode tcp` if yours do not.)

2. **Verify the balancer before touching kubelet.** Wait at least
   `health-interval × fall` (6 s by default) after starting the service
   before judging the results: backends start optimistically healthy, so
   a dead `--servers` entry only shows up after the first fall window.

   ```console
   w1$ systemctl is-active apiserver-lb              # active
   w1$ curl -sS http://127.0.0.1:9299/readyz         # ok (a connection error
                                                     # here means --metrics-listen
                                                     # is missing or wrong)
   w1$ curl -sS http://127.0.0.1:9299/metrics | grep backend_healthy
   # every configured server must report 1; a 0 means that backend is
   # down or unreachable — stop and fix it before proceeding
   w1$ curl -sSk https://127.0.0.1:6443/version      # apiserver version JSON.
   # On clusters with --anonymous-auth=false this returns a 401 Status
   # object instead — that is fine: either response comes from a real
   # apiserver and proves proxying works end to end.
   w1$ sudo journalctl -u apiserver-lb -n 20         # no unhealthy messages
   ```

3. **Back up, then switch kubelet:**

   ```console
   w1$ sudo cp /etc/kubernetes/kubelet.conf /etc/kubernetes/kubelet.conf.pre-lb
   ```

   - *Path A*: add the `/etc/hosts` line (Phase 1A). `kubelet.conf` is
     untouched.
   - *Path B*: edit `/etc/kubernetes/kubelet.conf`, set
     `server: https://127.0.0.1:6443`.

   ```console
   w1$ sudo systemctl restart kubelet
   ```

4. **Verify the node before proceeding to the next one:**

   ```console
   $ kubectl wait --for=condition=Ready node/w1 --timeout=120s
   $ kubectl get --raw /api/v1/nodes/w1/proxy/healthz   # kubelet reachable
   w1$ curl -s http://127.0.0.1:9299/metrics | grep connections_total
   # connections now flowing through the balancer
   ```

5. Repeat for the next worker only after step 4 passes.

### Rollback (single node)

```console
# Path A: remove the /etc/hosts line; Path B: restore the backup:
w1$ sudo cp /etc/kubernetes/kubelet.conf.pre-lb /etc/kubernetes/kubelet.conf
w1$ sudo systemctl restart kubelet
w1$ sudo systemctl disable --now apiserver-lb
$ kubectl wait --for=condition=Ready node/w1 --timeout=120s
```

The balancer never modifies cluster state, so rollback is purely local
to the node.

---

## Phase 5 — Implement the external endpoint

Put the Phase 2 decision in place (DNS records / external LB /
re-scoped kube-vip) and verify:

- external `kubectl` works against the new endpoint;
- `cluster-info` (Phase 3) points at it.

---

## Phase 6 — Remove kube-vip

Only after **every** worker is migrated and verified, and Phase 5 is
done:

```console
# 1. Remove the static pod on each CP (this is what stops the VIP):
cp1$ sudo rm /etc/kubernetes/manifests/kube-vip.yaml     # repeat cp2, cp3

# 2. Confirm nothing answers on the VIP anymore, and no worker cares:
$ kubectl get nodes    # all Ready

# 3. Clean up kube-vip's cluster objects (names may vary; discover with
#    kubectl get clusterrolebindings,clusterroles,sa -A | grep -i kube-vip):
$ kubectl -n kube-system delete lease plndr-cp-lock
$ kubectl -n kube-system delete sa kube-vip
$ kubectl delete clusterrolebinding system:kube-vip-binding
$ kubectl delete clusterrole system:kube-vip-role
```

If any worker was missed, its kubelet loses the apiserver the moment the
VIP disappears — `kubectl get nodes` shows it `NotReady` within the
node-monitor grace period. Re-check the full worker list before step 1.

---

## Phase 7 — Failover drill (do this before production traffic)

Prove the failover path end to end on a migrated (staging or canary)
worker:

1. Find which CP the worker's connections currently go to:

   ```console
   w1$ curl -s http://127.0.0.1:9299/metrics | grep backend_connections
   ```

2. On **that** CP, stop the apiserver the hard way (static pod removal —
   no graceful TCP FIN for established connections is exactly the
   scenario draining exists for):

   ```console
   cp2$ sudo mv /etc/kubernetes/manifests/kube-apiserver.yaml /root/
   ```

3. Watch the worker's balancer log. Within `health-interval × fall`
   (default 3s × 2 = 6s, plus one probe timeout worst-case) you must
   see:

   ```
   level=WARN msg="backend transitioned to unhealthy" backend=10.0.0.12:6443 ...
   level=WARN msg="drained active connections from unhealthy backend" backend=10.0.0.12:6443 connections=N
   ```

4. Verify the node never leaves Ready and the drained connections
   reconnected elsewhere:

   ```console
   $ kubectl get node w1 -w        # stays Ready throughout
   w1$ curl -s http://127.0.0.1:9299/metrics | grep backend_connections
   # connections now on the surviving CPs
   ```

5. Restore the apiserver and watch the backend return after `rise`
   consecutive successful probes (existing connections are deliberately
   not rebalanced onto it):

   ```console
   cp2$ sudo mv /root/kube-apiserver.yaml /etc/kubernetes/manifests/
   ```

If step 3 or 4 does not match, stop and investigate before migrating
further nodes — do not proceed on hope.
