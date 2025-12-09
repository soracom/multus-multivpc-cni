package kube

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PodEventHandler receives informer events restricted to the local node.
type PodEventHandler interface {
	OnPodAdd(ctx context.Context, pod *corev1.Pod)
	OnPodUpdate(ctx context.Context, oldPod, newPod *corev1.Pod)
	OnPodDelete(ctx context.Context, pod *corev1.Pod)
}

// PodWatcher encapsulates the shared informer for Pods scheduled to the node.
type PodWatcher struct {
	informer cache.SharedIndexInformer
	handler  PodEventHandler
}

// NewPodWatcher constructs a node-scoped Pod informer.
func NewPodWatcher(client kubernetes.Interface, nodeName string, handler PodEventHandler) (*PodWatcher, error) {
	lw := cache.NewListWatchFromClient(
		client.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
	)

	informer := cache.NewSharedIndexInformer(
		lw,
		&corev1.Pod{},
		5*time.Minute,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	pw := &PodWatcher{informer: informer, handler: handler}

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				handler.OnPodAdd(context.Background(), pod)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, oldOK := oldObj.(*corev1.Pod)
			newPod, newOK := newObj.(*corev1.Pod)
			if oldOK && newOK {
				handler.OnPodUpdate(context.Background(), oldPod, newPod)
			}
		},
		DeleteFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				handler.OnPodDelete(context.Background(), pod)
			}
		},
	}); err != nil {
		return nil, err
	}

	return pw, nil
}

// Run starts the informer until context cancellation.
func (w *PodWatcher) Run(ctx context.Context) {
	go w.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced) {
		return
	}
	<-ctx.Done()
}
