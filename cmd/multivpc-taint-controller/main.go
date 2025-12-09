package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/soracom/multus-multivpc-cni/pkg/taintcontroller"
)

type config struct {
	taintKey           string
	taintValue         string
	taintEffect        corev1.TaintEffect
	watchOnlyTainted   bool
	targetNodeSelector string
	agentNamespace     string
	agentPodSelector   string
	pollInterval       time.Duration
}

func main() {
	flag.Parse()

	cfg, err := loadConfigFromEnv()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	client, err := buildKubeClient()
	if err != nil {
		log.Fatalf("initialise kubernetes client: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("starting multivpc taint controller (taint=%s=%s:%s, watchOnlyTainted=%t, poll=%s, nodeSelector=%q, agentPods=%s/%s)",
		cfg.taintKey, cfg.taintValue, cfg.taintEffect, cfg.watchOnlyTainted, cfg.pollInterval, cfg.targetNodeSelector, cfg.agentNamespace, cfg.agentPodSelector)

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		if err := reconcile(ctx, client, cfg); err != nil {
			log.Printf("reconcile error: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Println("stopping taint controller")
			return
		case <-ticker.C:
		}
	}
}

func loadConfigFromEnv() (config, error) {
	cfg := config{
		taintKey:         getenvDefault("TAINT_KEY", "multus/not-ready"),
		taintValue:       getenvDefault("TAINT_VALUE", "true"),
		taintEffect:      corev1.TaintEffectNoSchedule,
		watchOnlyTainted: parseBool(getenvDefault("WATCH_ONLY_TAINTED", "false")),
		agentNamespace:   getenvDefault("AGENT_NAMESPACE", "kube-system"),
		agentPodSelector: getenvDefault("AGENT_POD_SELECTOR", "app.kubernetes.io/name=multus-multivpc-cni"),
		pollInterval:     parseDuration(getenvDefault("POLL_INTERVAL", "10s")),
	}

	if key := strings.TrimSpace(cfg.taintKey); key == "" {
		return cfg, errors.New("TAINT_KEY must be set")
	}
	if eff := os.Getenv("TAINT_EFFECT"); eff != "" {
		switch strings.TrimSpace(strings.ToLower(eff)) {
		case "noschedule":
			cfg.taintEffect = corev1.TaintEffectNoSchedule
		case "prefernoschedule":
			cfg.taintEffect = corev1.TaintEffectPreferNoSchedule
		case "noexecute":
			cfg.taintEffect = corev1.TaintEffectNoExecute
		default:
			return cfg, fmt.Errorf("invalid TAINT_EFFECT %q", eff)
		}
	}

	if sel := strings.TrimSpace(os.Getenv("TARGET_NODE_SELECTOR")); sel != "" {
		cfg.targetNodeSelector = sel
	}

	if cfg.pollInterval <= 0 {
		cfg.pollInterval = 10 * time.Second
	}

	return cfg, nil
}

func buildKubeClient() (*kubernetes.Clientset, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil && kubeconfig != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func reconcile(ctx context.Context, client *kubernetes.Clientset, cfg config) error {
	readyMap, err := buildAgentReadyMap(ctx, client, cfg.agentNamespace, cfg.agentPodSelector)
	if err != nil {
		return fmt.Errorf("list agent pods: %w", err)
	}

	nodeListOpts := metav1.ListOptions{}
	if cfg.targetNodeSelector != "" {
		nodeListOpts.LabelSelector = cfg.targetNodeSelector
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, nodeListOpts)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		manage := !cfg.watchOnlyTainted || taintcontroller.HasTaint(&node, cfg.taintKey, cfg.taintValue, cfg.taintEffect)
		if !manage {
			continue
		}
		ready := readyMap[node.Name]
		if err := syncNodeTaint(ctx, client, node.Name, ready, cfg); err != nil {
			log.Printf("sync node %s failed: %v", node.Name, err)
		}
	}
	return nil
}

func buildAgentReadyMap(ctx context.Context, client *kubernetes.Clientset, namespace, selector string) (map[string]bool, error) {
	ready := map[string]bool{}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: fields.Everything().String(),
	})
	if err != nil {
		return ready, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		if isPodReady(&pod) {
			ready[pod.Spec.NodeName] = true
		}
	}
	return ready, nil
}

func syncNodeTaint(ctx context.Context, client *kubernetes.Clientset, nodeName string, agentReady bool, cfg config) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		var changed bool
		if agentReady {
			changed = taintcontroller.RemoveTaint(node, cfg.taintKey, cfg.taintValue, cfg.taintEffect)
		} else {
			changed = taintcontroller.EnsureTaint(node, cfg.taintKey, cfg.taintValue, cfg.taintEffect)
		}

		if !changed {
			return nil
		}

		action := "applied"
		if agentReady {
			action = "removed"
		}
		log.Printf("node %s: agentReady=%t -> %s taint %s=%s:%s", nodeName, agentReady, action, cfg.taintKey, cfg.taintValue, cfg.taintEffect)

		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func getenvDefault(key, def string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	return val
}

func parseBool(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseDuration(val string) time.Duration {
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0
	}
	return d
}
