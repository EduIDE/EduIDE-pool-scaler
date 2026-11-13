package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/budget"
)

func pod(name, app, state string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"app": app, PoolStateLabel: state}},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func appDef(name string, targetIdle any) *unstructured.Unstructured {
	spec := map[string]any{
		"minInstances": int64(0), "maxInstances": int64(50),
		"requestsCpu": "500m", "requestsMemory": "500M",
		"sidecars": []any{map[string]any{"name": "lsp", "cpuRequest": "100m", "memoryRequest": "100M"}},
	}
	if targetIdle != nil {
		spec["targetIdle"] = targetIdle
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "theia.cloud/v1beta11", "kind": "AppDefinition",
		"metadata": map[string]any{"name": name, "namespace": "ns", "annotations": map[string]any{"x": "y"}},
		"spec":     spec,
	}}
	return u
}

func provider(objs ...runtime.Object) (*Provider, *dynfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{AppDefinitionGVR: "AppDefinitionList"},
		appDef("java-17", int64(4)), appDef("java-17-templates", nil))
	return &Provider{
		Dynamic: dyn, Core: fake.NewClientset(objs...), Namespace: "ns",
		PodOverhead: budget.Resources{CPU: 0.1, Memory: 128 << 20},
	}, dyn
}

func TestAppsCountsPoolPods(t *testing.T) {
	p, _ := provider(
		pod("instance-1-java-17-a", "java-17-1", "idle", true),
		pod("instance-2-java-17-a", "java-17-2", "idle", false),
		pod("instance-3-java-17-a", "java-17-3", "claimed", true),
		pod("instance-1-java-17-templates-a", "java-17-templates-1", "idle", true),
	)
	apps, err := p.Apps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j := apps["java-17"]
	if j.IdleReady != 1 || j.IdleStarting != 1 || j.Claimed != 1 {
		t.Fatalf("java-17 %+v", j)
	}
	if apps["java-17-templates"].IdleReady != 1 || apps["java-17-templates"].Claimed != 0 {
		t.Fatalf("templates %+v", apps["java-17-templates"])
	}
	if j.TargetIdle == nil || *j.TargetIdle != 4 || apps["java-17-templates"].TargetIdle != nil {
		t.Fatal("targetIdle not read")
	}
	if j.MaxInstances != 50 || j.Annotations["x"] != "y" {
		t.Fatalf("%+v", j)
	}
	if d := j.PodCPU - 0.7; d > 1e-9 || d < -1e-9 {
		t.Fatalf("pod cpu %v, want 0.5 + 0.1 sidecar + 0.1 overhead", j.PodCPU)
	}
	if j.PodMemory != 600e6+128<<20 {
		t.Fatalf("pod memory %v", j.PodMemory)
	}
}

func TestPatchTargetIdle(t *testing.T) {
	p, _ := provider()
	if err := p.PatchTargetIdle(context.Background(), "java-17-templates", 7); err != nil {
		t.Fatal(err)
	}
	apps, _ := p.Apps(context.Background())
	if v := apps["java-17-templates"].TargetIdle; v == nil || *v != 7 {
		t.Fatalf("got %v", v)
	}
	if apps["java-17-templates"].MaxInstances != 50 {
		t.Fatal("patch must leave other fields alone")
	}
}

func TestQuotaAndNodeRest(t *testing.T) {
	q := corev1.ResourceQuota{Status: corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("200"), corev1.ResourceRequestsStorage: resource.MustParse("500Gi")},
		Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("6200m")},
	}}
	r := QuotaRest([]corev1.ResourceQuota{q})
	if r.CPU != 193.8 || r.Memory != budget.Unlimited.Memory {
		t.Fatalf("%+v", r)
	}
	if QuotaRest(nil) != nil {
		t.Fatal("no quota means no limit")
	}

	node := func(name string, unschedulable bool) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: unschedulable},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16"), corev1.ResourceMemory: resource.MustParse("32Gi")}}}
	}
	running := func(nodeName string) corev1.Pod {
		return corev1.Pod{Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}}}}}}
	}
	n := NodeRest([]corev1.Node{node("a", false), node("b", true)}, []corev1.Pod{running("a"), running("b"), running("elsewhere")})
	if n.CPU != 14 || n.Memory != 28*(1<<30) {
		t.Fatalf("%+v", n)
	}
}
