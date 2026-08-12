// Package discovery keeps the backend pool in sync with the control
// plane by periodically reading the default/kubernetes Endpoints object,
// the same source RKE2-style agents use. The classic Endpoints resource
// (not EndpointSlice) is used deliberately: the Node authorizer allows
// kubelet credentials to read it, so both a dedicated ServiceAccount and
// the node's own kubelet.conf work as --discovery-kubeconfig.
//
// The static --servers list remains the bootstrap seed and fallback:
// discovery only ever narrows or widens the pool after the balancer is
// already serving. Requests are sent directly to backends from the pool
// (healthy first), never to the kubeconfig's server URL, so discovery
// keeps working as long as any known apiserver is reachable and cannot
// loop back through the balancer itself.
package discovery

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
	"github.com/cooloo9871/local-apiserver-lb/internal/kubeconfig"
)

const endpointsPath = "/api/v1/namespaces/default/endpoints/kubernetes"

// Options configures a Poller.
type Options struct {
	// KubeconfigPath is the credentials source. The file may not exist
	// yet at startup (pre-join kubelet.conf): discovery stays dormant
	// and retries loading it every interval.
	KubeconfigPath string
	Interval       time.Duration
	Timeout        time.Duration // per-fetch timeout
	// Validate vets a discovered server list before it is applied; the
	// caller wires the same validation used for --servers.
	Validate func(servers []string) error
	Logger   *slog.Logger
}

// Poller periodically refreshes the pool's backend set.
type Poller struct {
	pool *backend.Pool
	opts Options

	auth   *kubeconfig.Auth
	client *http.Client

	loggedWaiting bool
	loggedEmpty   bool
}

// New builds a Poller; call Run to start it.
func New(pool *backend.Pool, opts Options) *Poller {
	return &Poller{pool: pool, opts: opts}
}

// Run polls until ctx is canceled. The first attempt fires immediately.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()

	for {
		p.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollOnce performs one discovery round: ensure credentials, fetch the
// endpoints from any reachable backend, and reconcile the pool.
func (p *Poller) pollOnce(ctx context.Context) {
	if p.auth == nil {
		auth, err := kubeconfig.Load(p.opts.KubeconfigPath)
		if err != nil {
			if !p.loggedWaiting {
				p.opts.Logger.Info("discovery is waiting for a usable kubeconfig",
					"kubeconfig", p.opts.KubeconfigPath, "reason", err)
				p.loggedWaiting = true
			}
			return
		}
		p.auth = auth
		p.client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:              auth.CAPool,
					GetClientCertificate: auth.GetClientCertificate,
				},
				DisableKeepAlives: true,
			},
		}
		p.opts.Logger.Info("discovery activated", "kubeconfig", p.opts.KubeconfigPath)
	}

	addrs, err := p.fetch(ctx)
	if err != nil {
		p.opts.Logger.Debug("discovery fetch failed; keeping current backends", "error", err)
		return
	}
	p.apply(addrs)
}

// fetch retrieves and parses the endpoints object from the first backend
// that answers, in balancer preference order.
func (p *Poller) fetch(ctx context.Context) ([]string, error) {
	candidates, _ := p.pool.Candidates()
	var lastErr error
	for _, b := range candidates {
		addrs, err := p.fetchFrom(ctx, b.Addr())
		if err == nil {
			return addrs, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no backends to query")
	}
	return nil, lastErr
}

func (p *Poller) fetchFrom(ctx context.Context, addr string) ([]string, error) {
	fctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, "https://"+addr+endpointsPath, nil)
	if err != nil {
		return nil, err
	}
	if p.auth.BearerToken != nil {
		token, err := p.auth.BearerToken()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s from %s: status %d", endpointsPath, addr, resp.StatusCode)
	}

	var eps struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"subsets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&eps); err != nil {
		return nil, fmt.Errorf("decoding endpoints object: %w", err)
	}

	seen := map[string]bool{}
	var addrs []string
	for _, subset := range eps.Subsets {
		for _, a := range subset.Addresses {
			for _, port := range subset.Ports {
				hp := net.JoinHostPort(a.IP, strconv.Itoa(port.Port))
				if !seen[hp] {
					seen[hp] = true
					addrs = append(addrs, hp)
				}
			}
		}
	}
	sort.Strings(addrs)
	return addrs, nil
}

// apply reconciles the pool with a discovered address list, guarding
// against the two dangerous inputs: an empty list (a transient empty
// endpoints object must never wipe the pool) and a list that fails the
// same validation as --servers.
func (p *Poller) apply(addrs []string) {
	if len(addrs) == 0 {
		if !p.loggedEmpty {
			p.opts.Logger.Warn("discovery returned an empty endpoint list; keeping current backends")
			p.loggedEmpty = true
		}
		return
	}
	p.loggedEmpty = false

	if sameAddrs(addrs, p.pool.Backends()) {
		return
	}
	if err := p.opts.Validate(addrs); err != nil {
		p.opts.Logger.Warn("discovered server list rejected; keeping current backends",
			"servers", addrs, "error", err)
		return
	}

	added, removed := p.pool.SetAddrs(addrs)
	p.opts.Logger.Info("discovery updated backend servers", "servers", addrs)
	for _, b := range added {
		p.opts.Logger.Info("backend added by discovery", "backend", b.Addr())
	}
	for _, b := range removed {
		drained := b.DrainAll()
		p.opts.Logger.Info("backend removed by discovery",
			"backend", b.Addr(), "drained_connections", drained)
	}
}

// sameAddrs reports whether the sorted discovered list matches the
// current backend set.
func sameAddrs(addrs []string, backends []*backend.Backend) bool {
	if len(addrs) != len(backends) {
		return false
	}
	current := make(map[string]bool, len(backends))
	for _, b := range backends {
		current[b.Addr()] = true
	}
	for _, a := range addrs {
		if !current[a] {
			return false
		}
	}
	return true
}
