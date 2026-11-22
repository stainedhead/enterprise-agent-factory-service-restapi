// Package podcache maps a source IP to the team pod that owns it (gateway contract §3, M3-A2). It
// watches Namespaces (the team namespaces), Pods and, when configured, CiliumEndpoints, and keeps the
// answer in memory so a model call costs one map lookup. A lost watch keeps serving the cache while the
// informer re-lists; readiness waits for the first full list.
//
// The pod is identified by what the reconciler stamped on it: the team from the team namespace's label,
// the agent or pool from the pod's labels. A pod that claims another team than its namespace's is
// ignored, so labels alone never move a pod into a team.
package podcache

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// CiliumEndpointGVR is the resource whose per-endpoint identity binds an IP to a pod.
var CiliumEndpointGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints"}

// Config configures a Cache.
type Config struct {
	Client kubernetes.Interface
	// Dynamic, when set, adds the Cilium endpoint cross-check: a pod is accepted for an IP only when
	// Cilium's endpoint for that IP names the same namespace and pod.
	Dynamic dynamic.Interface
	Resync  time.Duration
	Log     *slog.Logger
}

type podRef struct {
	namespace, name, uid, serviceAccount string
	agentID, poolID, teamLabel           string
}

type endpointRef struct{ namespace, name string }

// Cache implements gateway.PodDirectory.
type Cache struct {
	cfg Config
	log *slog.Logger

	mu         sync.RWMutex
	byIP       map[netip.Addr]podRef
	ipsOfPod   map[string][]netip.Addr // namespace/name -> ips, to drop a pod's entries
	namespaces map[string]string       // team namespace -> team id
	endpoints  map[netip.Addr]endpointRef
	epsOfObj   map[string][]netip.Addr

	synced []cache.InformerSynced
}

// New returns a Cache; call Run to start it.
func New(cfg Config) (*Cache, error) {
	if cfg.Client == nil {
		return nil, errors.New("podcache: a Kubernetes client is required")
	}
	if cfg.Resync <= 0 {
		cfg.Resync = 10 * time.Minute
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Cache{cfg: cfg, log: log, byIP: map[netip.Addr]podRef{}, ipsOfPod: map[string][]netip.Addr{}, namespaces: map[string]string{},
		endpoints: map[netip.Addr]endpointRef{}, epsOfObj: map[string][]netip.Addr{}}, nil
}

var _ gateway.PodDirectory = (*Cache)(nil)

// Run starts the informers and blocks until ctx ends.
func (c *Cache) Run(ctx context.Context) error {
	f := informers.NewSharedInformerFactoryWithOptions(c.cfg.Client, c.cfg.Resync)
	nsInf := f.Core().V1().Namespaces().Informer()
	podInf := f.Core().V1().Pods().Informer()
	if err := podInf.SetTransform(slimPod); err != nil {
		return err
	}
	if _, err := nsInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.nsUpsert, UpdateFunc: func(_, n any) { c.nsUpsert(n) }, DeleteFunc: c.nsDelete}); err != nil {
		return err
	}
	if _, err := podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.podUpsert, UpdateFunc: func(_, n any) { c.podUpsert(n) }, DeleteFunc: c.podDelete}); err != nil {
		return err
	}
	synced := []cache.InformerSynced{nsInf.HasSynced, podInf.HasSynced}

	var df dynamicinformer.DynamicSharedInformerFactory
	if c.cfg.Dynamic != nil {
		df = dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.cfg.Dynamic, c.cfg.Resync, metav1.NamespaceAll, nil)
		epInf := df.ForResource(CiliumEndpointGVR).Informer()
		if _, err := epInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: c.epUpsert, UpdateFunc: func(_, n any) { c.epUpsert(n) }, DeleteFunc: c.epDelete}); err != nil {
			return err
		}
		synced = append(synced, epInf.HasSynced)
	}
	c.mu.Lock()
	c.synced = synced
	c.mu.Unlock()
	f.Start(ctx.Done())
	if df != nil {
		df.Start(ctx.Done())
	}
	<-ctx.Done()
	return nil
}

// slimPod drops what the cache never reads, so thousands of pods stay small.
func slimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, UID: p.UID, Labels: p.Labels},
		Spec:       corev1.PodSpec{ServiceAccountName: p.Spec.ServiceAccountName, HostNetwork: p.Spec.HostNetwork},
		Status:     corev1.PodStatus{Phase: p.Status.Phase, PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs},
	}, nil
}

// Ready implements gateway.PodDirectory: true once every informer has listed.
func (c *Cache) Ready() bool {
	c.mu.RLock()
	synced := c.synced
	c.mu.RUnlock()
	if len(synced) == 0 {
		return false
	}
	for _, s := range synced {
		if !s() {
			return false
		}
	}
	return true
}

// Lookup implements gateway.PodDirectory.
func (c *Cache) Lookup(ip netip.Addr) (gateway.PodInfo, bool) {
	ip = ip.Unmap()
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.byIP[ip]
	if !ok {
		return gateway.PodInfo{}, false
	}
	team, ok := c.namespaces[p.namespace]
	if !ok || (p.teamLabel != "" && p.teamLabel != team) {
		return gateway.PodInfo{}, false
	}
	if c.cfg.Dynamic != nil {
		// Cilium's endpoint identity must name the same pod as the Kubernetes pod with this IP.
		ep, ok := c.endpoints[ip]
		if !ok || ep.namespace != p.namespace || ep.name != p.name {
			return gateway.PodInfo{}, false
		}
	}
	return gateway.PodInfo{Namespace: p.namespace, Name: p.name, UID: p.uid, ServiceAccount: p.serviceAccount,
		TeamID: team, AgentID: p.agentID, PoolID: p.poolID}, true
}

// Len returns the number of IPs the cache knows, for a gauge.
func (c *Cache) Len() int { c.mu.RLock(); defer c.mu.RUnlock(); return len(c.byIP) }

func (c *Cache) nsUpsert(obj any) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	team := ns.Labels[render.LabelTeamID]
	if ns.Labels[render.LabelTeamNamespace] != "true" || team == "" {
		delete(c.namespaces, ns.Name)
		return
	}
	c.namespaces[ns.Name] = team
}

func (c *Cache) nsDelete(obj any) {
	ns, ok := tombstone[*corev1.Namespace](obj)
	if !ok {
		return
	}
	c.mu.Lock()
	delete(c.namespaces, ns.Name)
	c.mu.Unlock()
}

func tombstone[T any](obj any) (T, bool) {
	if t, ok := obj.(T); ok {
		return t, true
	}
	var zero T
	d, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return zero, false
	}
	t, ok := d.Obj.(T)
	return t, ok
}

func key(ns, name string) string { return ns + "/" + name }

func (c *Cache) podUpsert(obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	k := key(p.Namespace, p.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropPod(k)
	if p.Spec.HostNetwork || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return // a host-network pod shares the node's IP; a finished pod has none
	}
	ref := podRef{namespace: p.Namespace, name: p.Name, uid: string(p.UID), serviceAccount: p.Spec.ServiceAccountName,
		agentID: p.Labels[render.LabelAgentID], poolID: p.Labels[render.LabelPoolID], teamLabel: p.Labels[render.LabelTeamID]}
	var ips []netip.Addr
	add := func(s string) {
		if a, err := netip.ParseAddr(s); err == nil {
			ips = append(ips, a.Unmap())
		}
	}
	if p.Status.PodIP != "" {
		add(p.Status.PodIP)
	}
	for _, ip := range p.Status.PodIPs {
		add(ip.IP)
	}
	for _, a := range ips {
		c.byIP[a] = ref
	}
	c.ipsOfPod[k] = ips
}

// dropPod removes a pod's entries; an IP now held by another pod (reuse) is left alone.
func (c *Cache) dropPod(k string) {
	for _, a := range c.ipsOfPod[k] {
		if cur, ok := c.byIP[a]; ok && key(cur.namespace, cur.name) == k {
			delete(c.byIP, a)
		}
	}
	delete(c.ipsOfPod, k)
}

func (c *Cache) podDelete(obj any) {
	p, ok := tombstone[*corev1.Pod](obj)
	if !ok {
		return
	}
	c.mu.Lock()
	c.dropPod(key(p.Namespace, p.Name))
	c.mu.Unlock()
}

func (c *Cache) epUpsert(obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	k := key(u.GetNamespace(), u.GetName())
	var ips []netip.Addr
	addrs, _, _ := unstructured.NestedSlice(u.Object, "status", "networking", "addressing")
	for _, a := range addrs {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		for _, f := range []string{"ipv4", "ipv6"} {
			if s, _ := m[f].(string); s != "" {
				if ip, err := netip.ParseAddr(s); err == nil {
					ips = append(ips, ip.Unmap())
				}
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropEndpoint(k)
	for _, ip := range ips {
		c.endpoints[ip] = endpointRef{namespace: u.GetNamespace(), name: u.GetName()}
	}
	c.epsOfObj[k] = ips
}

func (c *Cache) dropEndpoint(k string) {
	for _, ip := range c.epsOfObj[k] {
		if cur, ok := c.endpoints[ip]; ok && key(cur.namespace, cur.name) == k {
			delete(c.endpoints, ip)
		}
	}
	delete(c.epsOfObj, k)
}

func (c *Cache) epDelete(obj any) {
	u, ok := tombstone[*unstructured.Unstructured](obj)
	if !ok {
		return
	}
	c.mu.Lock()
	c.dropEndpoint(key(u.GetNamespace(), u.GetName()))
	c.mu.Unlock()
}
