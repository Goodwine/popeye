package k8s

//go:generate popeye gen

import (
	"fmt"

	"github.com/derailed/popeye/pkg/config"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
	mv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

var (
	supportedMetricsAPIVersions = []string{"v1beta1"}
	systemNS                    = []string{"kube-system", "kube-public"}
)

// Client represents a Kubernetes api server client.
type Client struct {
	*config.Config

	api kubernetes.Interface

	allPods map[string]v1.Pod
	allNSs  map[string]v1.Namespace
	eps     map[string]v1.Endpoints
	allCRBs map[string]rbacv1.ClusterRoleBinding
	allRBs  map[string]rbacv1.RoleBinding
	allCMs  map[string]v1.ConfigMap
	allSecs map[string]v1.Secret
	allSAs  map[string]v1.ServiceAccount
}

// NewClient returns a dialable api server configuration.
func NewClient(config *config.Config) *Client {
	return &Client{Config: config}
}

// DialOrDie returns an api server client connection or dies.
func (c *Client) DialOrDie() kubernetes.Interface {
	client, err := c.Dial()
	if err != nil {
		panic(err)
	}
	return client
}

// Dial returns a handle to api server.
func (c *Client) Dial() (kubernetes.Interface, error) {
	if c.api != nil {
		return c.api, nil
	}

	cfg, err := c.Config.RESTConfig()
	if err != nil {
		return nil, err
	}

	if c.api, err = kubernetes.NewForConfig(cfg); err != nil {
		return nil, err
	}
	return c.api, nil
}

// ClusterHasMetrics checks if metrics server is available on the cluster.
func (c *Client) ClusterHasMetrics() bool {
	srv, err := c.Dial()
	if err != nil {
		return false
	}
	apiGroups, err := srv.Discovery().ServerGroups()
	if err != nil {
		return false
	}

	for _, discoveredAPIGroup := range apiGroups.Groups {
		if discoveredAPIGroup.Name != metricsapi.GroupName {
			continue
		}
		for _, version := range discoveredAPIGroup.Versions {
			for _, supportedVersion := range supportedMetricsAPIVersions {
				if version.Version == supportedVersion {
					return true
				}
			}
		}
	}
	return false
}

// FetchNodesMetrics fetch all node metrics.
func (c *Client) FetchNodesMetrics() ([]mv1beta1.NodeMetrics, error) {
	return FetchNodesMetrics(c)
}

// FetchPodsMetrics fetch all pods metrics in a given namespace.
func (c *Client) FetchPodsMetrics(ns string) ([]mv1beta1.PodMetrics, error) {
	return FetchPodsMetrics(c, ns)
}

// InUseNamespaces returns a list of namespaces referenced by pods.
func (c *Client) InUseNamespaces(nss []string) {
	pods, err := c.ListPods()
	if err != nil {
		return
	}

	ll := make(map[string]struct{})
	for _, p := range pods {
		ll[p.Namespace] = struct{}{}
	}

	var i int
	for k := range ll {
		nss[i] = k
		i++
	}
}

// ListAllRBs returns all RoleBindings.
func (c *Client) ListAllRBs() (map[string]rbacv1.RoleBinding, error) {
	if c.allRBs != nil {
		return c.allRBs, nil
	}

	ll, err := c.DialOrDie().RbacV1().RoleBindings("").List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allRBs = make(map[string]rbacv1.RoleBinding, len(ll.Items))
	for _, rb := range ll.Items {
		c.allRBs[fqn(rb.Namespace, rb.Name)] = rb
	}

	return c.allRBs, nil
}

// ListRBs lists all available RBs in a given namespace.
func (c *Client) ListRBs() (map[string]rbacv1.RoleBinding, error) {
	rbs, err := c.ListAllRBs()
	if err != nil {
		return nil, err
	}

	res := make(map[string]rbacv1.RoleBinding, len(rbs))
	for fqn, rb := range rbs {
		if c.matchActiveNS(rb.Namespace) && !c.Config.ExcludedNS(rb.Namespace) {
			res[fqn] = rb
		}
	}

	return res, nil
}

// ListAllCRBs returns a ClusterRoleBindings.
func (c *Client) ListAllCRBs() (map[string]rbacv1.ClusterRoleBinding, error) {
	if c.allCRBs != nil {
		return c.allCRBs, nil
	}

	ll, err := c.DialOrDie().RbacV1().ClusterRoleBindings().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allCRBs = make(map[string]rbacv1.ClusterRoleBinding, len(ll.Items))
	for _, crb := range ll.Items {
		c.allCRBs[crb.Name] = crb
	}

	return c.allCRBs, nil
}

// ListEndpoints returns a endpoint by name.
func (c *Client) ListEndpoints() (map[string]v1.Endpoints, error) {
	if c.eps != nil {
		return c.eps, nil
	}

	ll, err := c.DialOrDie().CoreV1().Endpoints(c.Config.ActiveNamespace()).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.eps = make(map[string]v1.Endpoints, len(ll.Items))
	for _, ep := range ll.Items {
		if !c.Config.ExcludedNS(ep.Namespace) {
			c.eps[fqn(ep.Namespace, ep.Name)] = ep
		}
	}

	return c.eps, nil
}

// GetEndpoints returns a endpoint by name.
func (c *Client) GetEndpoints(svcFQN string) (*v1.Endpoints, error) {
	eps, err := c.ListEndpoints()
	if err != nil {
		return nil, err
	}

	if ep, ok := eps[svcFQN]; ok {
		return &ep, nil
	}

	return nil, fmt.Errorf("Unable to find ep for service %s", svcFQN)
}

// ListServices lists all available services in a given namespace.
func (c *Client) ListServices() ([]v1.Service, error) {
	ll, err := c.DialOrDie().CoreV1().Services(c.Config.ActiveNamespace()).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	svcs := make([]v1.Service, 0, len(ll.Items))
	for _, svc := range ll.Items {
		if c.matchActiveNS(svc.Namespace) && !c.Config.ExcludedNS(svc.Namespace) {
			svcs = append(svcs, svc)
		}
	}

	return svcs, nil
}

// ListNodes list all available nodes on the cluster.
func (c *Client) ListNodes() ([]v1.Node, error) {
	ll, err := c.DialOrDie().CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	nodes := make([]v1.Node, 0, len(ll.Items))
	for _, no := range ll.Items {
		if !c.Config.ExcludedNode(no.Name) {
			nodes = append(nodes, no)
		}
	}

	return nodes, nil
}

// GetPod returns a pod via a label query.
func (c *Client) GetPod(sel map[string]string) (*v1.Pod, error) {
	pods, err := c.ListPods()
	if err != nil {
		return nil, err
	}

	for _, po := range pods {
		var found int
		for k, v := range sel {
			if pv, ok := po.Labels[k]; ok && pv == v {
				found++
			}
		}
		if found == len(sel) {
			return &po, nil
		}
	}

	return nil, fmt.Errorf("No pods match service selector")
}

// ListPods list all available pods.
func (c *Client) ListPods() (map[string]v1.Pod, error) {
	pods, err := c.ListAllPods()
	if err != nil {
		return nil, err
	}

	res := make(map[string]v1.Pod, len(pods))
	for fqn, po := range pods {
		if c.matchActiveNS(po.Namespace) && !c.Config.ExcludedNS(po.Namespace) {
			res[fqn] = po
		}
	}

	return res, nil
}

// ListAllPods fetch all pods on the cluster.
func (c *Client) ListAllPods() (map[string]v1.Pod, error) {
	if len(c.allPods) != 0 {
		return c.allPods, nil
	}

	ll, err := c.DialOrDie().CoreV1().Pods("").List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allPods = make(map[string]v1.Pod, len(ll.Items))
	for _, po := range ll.Items {
		c.allPods[fqn(po.Namespace, po.Name)] = po
	}

	return c.allPods, nil
}

// ListCMs list all included ConfigMaps.
func (c *Client) ListCMs() (map[string]v1.ConfigMap, error) {
	cms, err := c.ListAllCMs()
	if err != nil {
		return nil, err
	}

	res := make(map[string]v1.ConfigMap, len(cms))
	for fqn, cm := range cms {
		if c.matchActiveNS(cm.Namespace) && !c.Config.ExcludedNS(cm.Namespace) {
			res[fqn] = cm
		}
	}

	return res, nil
}

// ListAllCMs fetch all configmaps on the cluster.
func (c *Client) ListAllCMs() (map[string]v1.ConfigMap, error) {
	if len(c.allCMs) != 0 {
		return c.allCMs, nil
	}

	ll, err := c.DialOrDie().CoreV1().ConfigMaps("").List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allCMs = make(map[string]v1.ConfigMap, len(ll.Items))
	for _, cm := range ll.Items {
		c.allCMs[fqn(cm.Namespace, cm.Name)] = cm
	}

	return c.allCMs, nil
}

// ListSecs list all included Secrets.
func (c *Client) ListSecs() (map[string]v1.Secret, error) {
	secs, err := c.ListAllSecs()
	if err != nil {
		return nil, err
	}

	res := make(map[string]v1.Secret, len(secs))
	for fqn, sec := range secs {
		if c.matchActiveNS(sec.Namespace) && !c.Config.ExcludedNS(sec.Namespace) {
			res[fqn] = sec
		}
	}

	return res, nil
}

// ListAllSecs fetch all secrets on the cluster.
func (c *Client) ListAllSecs() (map[string]v1.Secret, error) {
	if len(c.allSecs) != 0 {
		return c.allSecs, nil
	}

	ll, err := c.DialOrDie().CoreV1().Secrets("").List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allSecs = make(map[string]v1.Secret, len(ll.Items))
	for _, sec := range ll.Items {
		c.allSecs[fqn(sec.Namespace, sec.Name)] = sec
	}

	return c.allSecs, nil
}

// ListSAs list all included ConfigMaps.
func (c *Client) ListSAs() (map[string]v1.ServiceAccount, error) {
	sas, err := c.ListAllSAs()
	if err != nil {
		return nil, err
	}

	res := make(map[string]v1.ServiceAccount, len(sas))
	for fqn, sa := range sas {
		if c.matchActiveNS(sa.Namespace) && !c.Config.ExcludedNS(sa.Namespace) {
			res[fqn] = sa
		}
	}

	return res, nil
}

// ListAllSAs fetch all ServiceAccount on the cluster.
func (c *Client) ListAllSAs() (map[string]v1.ServiceAccount, error) {
	if len(c.allSAs) != 0 {
		return c.allSAs, nil
	}

	ll, err := c.DialOrDie().CoreV1().ServiceAccounts("").List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allSAs = make(map[string]v1.ServiceAccount, len(ll.Items))
	for _, sa := range ll.Items {
		c.allSAs[fqn(sa.Namespace, sa.Name)] = sa
	}

	return c.allSAs, nil
}

// ListNS lists all available namespaces.
func (c *Client) ListNS() (map[string]v1.Namespace, error) {
	nss, err := c.ListAllNS()
	if err != nil {
		return nil, nil
	}

	res := make(map[string]v1.Namespace, len(nss))
	for n, ns := range nss {
		if c.matchActiveNS(n) && !c.Config.ExcludedNS(n) {
			res[n] = ns
		}
	}

	return res, nil
}

// ListAllNS fetch all namespaces on this cluster.
func (c *Client) ListAllNS() (map[string]v1.Namespace, error) {
	if len(c.allNSs) != 0 {
		return c.allNSs, nil
	}

	nn, err := c.DialOrDie().CoreV1().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	c.allNSs = make(map[string]v1.Namespace, len(nn.Items))
	for _, ns := range nn.Items {
		c.allNSs[ns.Name] = ns
	}

	return c.allNSs, nil
}

func (c *Client) matchActiveNS(ns string) bool {
	if c.Config.ActiveNamespace() == "" {
		return true
	}
	return ns == c.Config.ActiveNamespace()
}

// ----------------------------------------------------------------------------
// Helpers...

func fqn(ns, n string) string {
	return ns + "/" + n
}

func isSystemNS(ns string) bool {
	for _, n := range systemNS {
		if n == ns {
			return true
		}
	}
	return false
}
