// Package kubernetes reads the pool state, pod sizes, quota and node capacity,
// and patches spec.targetIdle. It is the scaler's only writer.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/budget"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// AppDefinitionGVR is the served AppDefinition version.
var AppDefinitionGVR = schema.GroupVersionResource{Group: "theia.cloud", Version: "v1beta11", Resource: "appdefinitions"}

// PoolStateLabel is set by the operator on pool Deployments, Services and Pods.
const PoolStateLabel = "pool-state"

// Provider talks to the Kubernetes API.
type Provider struct {
	Dynamic      dynamic.Interface
	Core         kubernetes.Interface
	Namespace    string
	NodeSelector map[string]string
	PodOverhead  budget.Resources
}

// Apps returns every AppDefinition with its pool counts and pod size.
func (p *Provider) Apps(ctx context.Context) (map[string]*model.App, error) {
	list, err := p.Dynamic.Resource(AppDefinitionGVR).Namespace(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list appdefinitions: %w", err)
	}
	apps := map[string]*model.App{}
	for i := range list.Items {
		a, err := p.appFromUnstructured(&list.Items[i])
		if err != nil {
			return nil, err
		}
		apps[a.Name] = a
	}

	pods, err := p.Core.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{LabelSelector: PoolStateLabel})
	if err != nil {
		return nil, fmt.Errorf("list pool pods: %w", err)
	}
	CountPods(apps, pods.Items)
	return apps, nil
}

func (p *Provider) appFromUnstructured(u *unstructured.Unstructured) (*model.App, error) {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	a := &model.App{
		Name:        u.GetName(),
		Annotations: u.GetAnnotations(),
		ClaimRate:   -1,
	}
	if v, ok, _ := unstructured.NestedInt64(spec, "minInstances"); ok {
		a.MinInstances = int(v)
	}
	if v, ok, _ := unstructured.NestedInt64(spec, "maxInstances"); ok {
		a.MaxInstances = int(v)
	}
	if v, ok, _ := unstructured.NestedInt64(spec, "targetIdle"); ok {
		t := int(v)
		a.TargetIdle = &t
	}
	a.PodCPU = p.PodOverhead.CPU + quantity(spec["requestsCpu"])
	a.PodMemory = p.PodOverhead.Memory + quantity(spec["requestsMemory"])
	sidecars, _, _ := unstructured.NestedSlice(spec, "sidecars")
	for _, s := range sidecars {
		if m, ok := s.(map[string]any); ok {
			a.PodCPU += quantity(m["cpuRequest"])
			a.PodMemory += quantity(m["memoryRequest"])
		}
	}
	return a, nil
}

func quantity(v any) float64 {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case int64:
		return float64(t)
	case float64:
		return t
	default:
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.AsApproximateFloat64()
}

var appLabel = regexp.MustCompile(`^(.+)-\d+$`)

// CountPods sorts pool pods into idle ready, idle starting and claimed. The
// operator labels a pool pod `app=<appdefinition>-<instance number>`.
func CountPods(apps map[string]*model.App, pods []corev1.Pod) {
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		m := appLabel.FindStringSubmatch(pod.Labels["app"])
		if m == nil {
			continue
		}
		a, ok := apps[m[1]]
		if !ok {
			continue
		}
		switch pod.Labels[PoolStateLabel] {
		case "claimed":
			a.Claimed++
		case "idle":
			if podReady(pod) {
				a.IdleReady++
			} else {
				a.IdleStarting++
			}
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Budget returns the quota rest and the unrequested allocatable on the
// session nodes. nil means that source sets no limit.
func (p *Provider) Budget(ctx context.Context) (quota, nodes *budget.Resources, err error) {
	quotas, err := p.Core.CoreV1().ResourceQuotas(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list resourcequotas: %w", err)
	}
	quota = QuotaRest(quotas.Items)

	nodeList, err := p.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(p.NodeSelector).String()})
	if err != nil {
		return quota, nil, fmt.Errorf("list nodes: %w", err)
	}
	pods, err := p.Core.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
	if err != nil {
		return quota, nil, fmt.Errorf("list pods: %w", err)
	}
	nodes = NodeRest(nodeList.Items, pods.Items)
	return quota, nodes, nil
}

// QuotaRest is hard minus used for request quotas, the minimum over all
// quotas in the namespace.
func QuotaRest(quotas []corev1.ResourceQuota) *budget.Resources {
	if len(quotas) == 0 {
		return nil
	}
	rest := budget.Unlimited
	for _, q := range quotas {
		for _, pair := range []struct {
			names []corev1.ResourceName
			dst   *float64
		}{
			{[]corev1.ResourceName{corev1.ResourceRequestsCPU, corev1.ResourceCPU}, &rest.CPU},
			{[]corev1.ResourceName{corev1.ResourceRequestsMemory, corev1.ResourceMemory}, &rest.Memory},
		} {
			for _, n := range pair.names {
				hard, ok := q.Status.Hard[n]
				if !ok {
					hard, ok = q.Spec.Hard[n]
				}
				if !ok {
					continue
				}
				used := q.Status.Used[n]
				*pair.dst = math.Min(*pair.dst, hard.AsApproximateFloat64()-used.AsApproximateFloat64())
			}
		}
	}
	return &rest
}

// NodeRest is allocatable minus requested over the schedulable nodes given.
func NodeRest(nodes []corev1.Node, pods []corev1.Pod) *budget.Resources {
	if len(nodes) == 0 {
		return nil
	}
	in := map[string]bool{}
	var rest budget.Resources
	for _, n := range nodes {
		if n.Spec.Unschedulable {
			continue
		}
		in[n.Name] = true
		rest.CPU += n.Status.Allocatable.Cpu().AsApproximateFloat64()
		rest.Memory += n.Status.Allocatable.Memory().AsApproximateFloat64()
	}
	for _, pod := range pods {
		if !in[pod.Spec.NodeName] {
			continue
		}
		for _, c := range pod.Spec.Containers {
			rest.CPU -= c.Resources.Requests.Cpu().AsApproximateFloat64()
			rest.Memory -= c.Resources.Requests.Memory().AsApproximateFloat64()
		}
	}
	return &rest
}

// PatchTargetIdle sets spec.targetIdle with a merge patch, leaving every other
// field to its owner.
func (p *Provider) PatchTargetIdle(ctx context.Context, app string, v int) error {
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"targetIdle": v}})
	_, err := p.Dynamic.Resource(AppDefinitionGVR).Namespace(p.Namespace).Patch(ctx, app, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "pool-scaler"})
	return err
}
