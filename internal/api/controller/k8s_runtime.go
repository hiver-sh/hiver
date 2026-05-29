package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"encoding/json"

	gen "github.com/blasten/hive/internal/api/gen/controller"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// K8sRuntime implements SandboxRuntime using the Kubernetes API.
type K8sRuntime struct {
	client    kubernetes.Interface
	namespace string
}

func newK8sRuntime() (*K8sRuntime, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	ns := os.Getenv("HIVE_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &K8sRuntime{client: client, namespace: ns}, nil
}

func (r *K8sRuntime) Lookup(id string) (bool, gen.Sandbox, error) {
	name := containerNameFor(id)
	pod, err := r.client.CoreV1().Pods(r.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, gen.Sandbox{}, nil
		}
		return false, gen.Sandbox{}, fmt.Errorf("get pod %s: %w", name, err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false, gen.Sandbox{}, nil
	}
	ep := r.tcpProxyEndpoint(name)
	return true, gen.Sandbox{Id: id, Endpoint: r.endpointFor(name), ExposedEndpoint: &ep}, nil
}

func (r *K8sRuntime) List() ([]gen.Sandbox, error) {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSandboxID,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	sandboxes := make([]gen.Sandbox, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		id := pod.Labels[labelSandboxID]
		ep := r.tcpProxyEndpoint(pod.Name)
		sandboxes = append(sandboxes, gen.Sandbox{Id: id, Endpoint: r.endpointFor(pod.Name), ExposedEndpoint: &ep})
	}
	return sandboxes, nil
}

func (r *K8sRuntime) Get(id string) (gen.SandboxDetail, error) {
	name := containerNameFor(id)
	pod, err := r.client.CoreV1().Pods(r.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return gen.SandboxDetail{}, ErrSandboxNotFound
		}
		return gen.SandboxDetail{}, fmt.Errorf("get pod %s: %w", name, err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return gen.SandboxDetail{}, ErrSandboxNotFound
	}
	ep := r.tcpProxyEndpoint(name)
	cmd := fmt.Sprintf("kubectl exec -it -n %s %s -c sandbox -- sandbox-exec", r.namespace, name)
	return gen.SandboxDetail{
		Id:              id,
		Endpoint:        r.endpointFor(name),
		ExposedEndpoint: &ep,
		TerminalCmd:     &cmd,
	}, nil
}

func (r *K8sRuntime) Start(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	ctx := context.Background()
	name := containerNameFor(id)

	labels := map[string]string{labelSandboxID: id}

	specBytes, err := json.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Data:       map[string]string{"spec.json": string(specBytes)},
	}
	if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return gen.Sandbox{}, fmt.Errorf("create configmap: %w", err)
	}

	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: r.imageFor(cfg),
					Args:  []string{"--spec", "/mnt/spec.json"},
					Ports: []corev1.ContainerPort{
						{ContainerPort: sandboxAPIPort},
						{ContainerPort: sandboxTCPProxyPort},
					},
					Env: r.envVars(cfg),
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "spec", MountPath: "/mnt", ReadOnly: true},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "spec",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: name},
						},
					},
				},
			},
		},
	}
	if _, err := r.client.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		_ = r.client.CoreV1().ConfigMaps(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
		return gen.Sandbox{}, fmt.Errorf("create pod: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Port: sandboxAPIPort, Protocol: corev1.ProtocolTCP},
				{Port: sandboxTCPProxyPort, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if _, err := r.client.CoreV1().Services(r.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		_ = r.client.CoreV1().Pods(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
		_ = r.client.CoreV1().ConfigMaps(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
		return gen.Sandbox{}, fmt.Errorf("create service: %w", err)
	}

	ep := r.tcpProxyEndpoint(name)
	return gen.Sandbox{Id: id, Endpoint: r.endpointFor(name), ExposedEndpoint: &ep}, nil
}

func (r *K8sRuntime) Shutdown(id string) error {
	ctx := context.Background()
	name := containerNameFor(id)

	if _, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if k8serrors.IsNotFound(err) {
			return ErrSandboxNotFound
		}
		return fmt.Errorf("get pod %s: %w", name, err)
	}

	_ = r.client.CoreV1().Services(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = r.client.CoreV1().ConfigMaps(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err := r.client.CoreV1().Pods(r.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

func (r *K8sRuntime) endpointFor(name string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, r.namespace, sandboxAPIPort)
}

func (r *K8sRuntime) tcpProxyEndpoint(svcName string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", svcName, r.namespace, sandboxTCPProxyPort)
}

func (r *K8sRuntime) imageFor(cfg sandboxgen.SandboxConfig) string {
	if cfg.Image != nil && *cfg.Image != "" {
		return *cfg.Image
	}
	return defaultSandboxImage
}

func (r *K8sRuntime) envVars(cfg sandboxgen.SandboxConfig) []corev1.EnvVar {
	if cfg.Env == nil {
		return nil
	}
	vars := make([]corev1.EnvVar, 0, len(*cfg.Env))
	for k, v := range *cfg.Env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	return vars
}
