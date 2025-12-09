package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/soracom/multus-multivpc-cni/pkg/aws"
	syncconfig "github.com/soracom/multus-multivpc-cni/pkg/config"
	"github.com/soracom/multus-multivpc-cni/pkg/kube"
	syncpb "github.com/soracom/multus-multivpc-cni/proto/syncpb"
)

type jobKind string

const (
	jobKindEnsure  jobKind = "ensure"
	jobKindRelease jobKind = "release"
)

type syncJob struct {
	kind       jobKind
	profile    syncconfig.NetworkProfile
	profileKey string
	ensureReq  aws.SyncRequest
	relReq     aws.ReleaseRequest
}

type eniManager interface {
	EnsureInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req aws.SyncRequest) (*aws.SyncResult, error)
	ReleaseInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req aws.ReleaseRequest) error
}

type agentMetrics struct {
	syncTotal      *prometheus.CounterVec
	syncFailures   *prometheus.CounterVec
	syncLatency    *prometheus.HistogramVec
	queueDepth     prometheus.Gauge
	inFlight       prometheus.Gauge
	releaseTotal   *prometheus.CounterVec
	releaseFailure *prometheus.CounterVec
}

type registererWithMust interface {
	prometheus.Registerer
	MustRegister(...prometheus.Collector)
}

func newAgentMetrics(reg prometheus.Registerer) *agentMetrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &agentMetrics{
		syncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "sync_total",
			Help:      "Total number of sync operations",
		}, []string{"profile"}),
		syncFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "sync_failures_total",
			Help:      "Number of failed sync operations",
		}, []string{"profile"}),
		syncLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "sync_duration_seconds",
			Help:      "Duration of ENI sync operations",
			Buckets:   prometheus.DefBuckets,
		}, []string{"profile"}),
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "queue_depth",
			Help:      "Current length of the sync queue",
		}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "inflight_requests",
			Help:      "Number of AWS requests currently in flight",
		}),
		releaseTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "release_total",
			Help:      "Total number of release operations",
		}, []string{"profile"}),
		releaseFailure: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multivpc",
			Subsystem: "eni_agent",
			Name:      "release_failures_total",
			Help:      "Number of failed release operations",
		}, []string{"profile"}),
	}

	collectors := []prometheus.Collector{
		m.syncTotal,
		m.syncFailures,
		m.syncLatency,
		m.queueDepth,
		m.inFlight,
		m.releaseTotal,
		m.releaseFailure,
	}

	if rm, ok := reg.(registererWithMust); ok {
		rm.MustRegister(collectors...)
	} else {
		for _, c := range collectors {
			if err := reg.Register(c); err != nil {
				panic(err)
			}
		}
	}

	return m
}

type agentServer struct {
	syncpb.UnimplementedIpSyncServiceServer

	manager eniManager
	cfg     *syncconfig.Config
	metrics *agentMetrics
	node    string

	queue chan syncJob

	stateMu sync.RWMutex
	state   map[string]*syncpb.InterfaceState
}

func newAgentServer(manager eniManager, cfg *syncconfig.Config, node string, metrics *agentMetrics, queueSize int) *agentServer {
	return &agentServer{
		manager: manager,
		cfg:     cfg,
		metrics: metrics,
		node:    node,
		queue:   make(chan syncJob, queueSize),
		state:   make(map[string]*syncpb.InterfaceState),
	}
}

func (a *agentServer) start(ctx context.Context) {
	go func() {
		for {
			select {
			case job := <-a.queue:
				a.metrics.queueDepth.Dec()
				a.handleJob(ctx, job)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (a *agentServer) handleJob(ctx context.Context, job syncJob) {
	switch job.kind {
	case jobKindEnsure:
		start := time.Now()
		a.metrics.inFlight.Inc()
		res, err := a.manager.EnsureInterface(ctx, job.profile, job.ensureReq)
		a.metrics.inFlight.Dec()
		if err != nil {
			log.Printf("sync failed for %s/%s: %v", job.ensureReq.Namespace, job.ensureReq.PodName, err)
			a.metrics.syncFailures.WithLabelValues(job.profileKey).Inc()
			return
		}
		a.metrics.syncTotal.WithLabelValues(job.profileKey).Inc()
		a.metrics.syncLatency.WithLabelValues(job.profileKey).Observe(time.Since(start).Seconds())
		a.updateState(job.ensureReq, res)
	case jobKindRelease:
		a.metrics.inFlight.Inc()
		err := a.manager.ReleaseInterface(ctx, job.profile, job.relReq)
		a.metrics.inFlight.Dec()
		if err != nil {
			log.Printf("release failed for %s/%s: %v", job.relReq.Namespace, job.relReq.PodName, err)
			a.metrics.releaseFailure.WithLabelValues(job.profileKey).Inc()
			return
		}
		a.metrics.releaseTotal.WithLabelValues(job.profileKey).Inc()
		a.deleteState(job.relReq)
	}
}

func (a *agentServer) updateState(req aws.SyncRequest, res *aws.SyncResult) {
	key := stateKey(req.PodUID, req.InterfaceName)
	state := &syncpb.InterfaceState{
		PodUid:         req.PodUID,
		PodName:        req.PodName,
		Namespace:      req.Namespace,
		NetworkProfile: req.NetworkProfile,
		EniId:          res.ENIID,
		Ipv4Addresses:  append([]string{}, res.AssignedIPv4...),
		Ipv6Addresses:  append([]string{}, res.AssignedIPv6...),
		Status:         "attached",
		InterfaceName:  req.InterfaceName,
		MacAddress:     res.MacAddress,
	}
	a.stateMu.Lock()
	a.state[key] = state
	a.stateMu.Unlock()
}

func (a *agentServer) deleteState(req aws.ReleaseRequest) {
	key := stateKey(req.PodUID, req.InterfaceName)
	a.stateMu.Lock()
	delete(a.state, key)
	a.stateMu.Unlock()
}

func stateKey(podUID, ifName string) string {
	return fmt.Sprintf("%s:%s", podUID, ifName)
}

func (a *agentServer) snapshotStates(filter func(*syncpb.InterfaceState) bool) []*syncpb.InterfaceState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()

	var states []*syncpb.InterfaceState
	for _, st := range a.state {
		if filter != nil && !filter(st) {
			continue
		}
		clone := proto.Clone(st)
		if cloned, ok := clone.(*syncpb.InterfaceState); ok {
			states = append(states, cloned)
		}
	}
	return states
}

func (a *agentServer) ReportInterface(ctx context.Context, req *syncpb.ReportRequest) (*syncpb.ReportResponse, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.NodeName != "" && req.NodeName != a.node {
		return nil, fmt.Errorf("request targeted for node %s", req.NodeName)
	}
	profile, ok := a.cfg.ResolveProfile(req.NetworkProfile)
	if !ok {
		return nil, fmt.Errorf("unknown network profile %q", req.NetworkProfile)
	}

	job := syncJob{
		kind:       jobKindEnsure,
		profile:    profile,
		profileKey: req.NetworkProfile,
		ensureReq: aws.SyncRequest{
			PodUID:         req.PodUid,
			PodName:        req.PodName,
			Namespace:      req.Namespace,
			InterfaceName:  req.InterfaceName,
			NetworkProfile: req.NetworkProfile,
			IPv4Addresses:  append([]string{}, req.Ipv4Addresses...),
			IPv6Addresses:  append([]string{}, req.Ipv6Addresses...),
			MacAddress:     req.MacAddress,
		},
	}

	enqueued := a.enqueueJob(job)
	msg := "queued"
	if !enqueued {
		msg = "queue full, dropped"
	}
	return &syncpb.ReportResponse{
		Accepted:     enqueued,
		Message:      msg,
		EniId:        "",
		AttachmentId: "",
		MacAddress:   "",
	}, nil
}

func (a *agentServer) ReleaseInterface(ctx context.Context, req *syncpb.ReleaseRequest) (*syncpb.ReleaseResponse, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	profile, ok := a.cfg.ResolveProfile(req.NetworkProfile)
	if !ok {
		return nil, fmt.Errorf("unknown network profile %q", req.NetworkProfile)
	}
	job := syncJob{
		kind:       jobKindRelease,
		profile:    profile,
		profileKey: req.NetworkProfile,
		relReq: aws.ReleaseRequest{
			PodUID:         req.PodUid,
			PodName:        req.PodName,
			Namespace:      req.Namespace,
			InterfaceName:  req.InterfaceName,
			NetworkProfile: req.NetworkProfile,
		},
	}
	enqueued := a.enqueueJob(job)
	msg := "queued"
	if !enqueued {
		msg = "queue full, dropped"
	}
	return &syncpb.ReleaseResponse{Accepted: enqueued, Message: msg}, nil
}

func (a *agentServer) GetStatus(ctx context.Context, req *syncpb.GetStatusRequest) (*syncpb.GetStatusResponse, error) {
	filter := func(st *syncpb.InterfaceState) bool {
		if req == nil {
			return true
		}
		if req.PodUid != "" && st.PodUid != req.PodUid {
			return false
		}
		if req.InterfaceName != "" && st.InterfaceName != req.InterfaceName {
			return false
		}
		if req.NetworkProfile != "" && st.NetworkProfile != req.NetworkProfile {
			return false
		}
		return true
	}
	states := a.snapshotStates(filter)
	return &syncpb.GetStatusResponse{Interfaces: states}, nil
}

func (a *agentServer) enqueueJob(job syncJob) bool {
	select {
	case a.queue <- job:
		a.metrics.queueDepth.Inc()
		return true
	default:
		log.Printf("queue full, dropping job for %s", job.profileKey)
		return false
	}
}

// Pod watcher hooks.

func (a *agentServer) OnPodAdd(_ context.Context, pod *corev1.Pod) {
	if isPodTerminating(pod) {
		a.enqueueReleasesForPod(pod)
	}
}

func (a *agentServer) OnPodUpdate(_ context.Context, oldPod, newPod *corev1.Pod) {
	if oldPod == nil || newPod == nil {
		return
	}
	if !isPodTerminating(oldPod) && isPodTerminating(newPod) {
		a.enqueueReleasesForPod(newPod)
	}
}

func (a *agentServer) OnPodDelete(_ context.Context, pod *corev1.Pod) {
	if pod == nil {
		return
	}
	a.enqueueReleasesForPod(pod)
}

func (a *agentServer) enqueueReleasesForPod(pod *corev1.Pod) {
	uid := string(pod.UID)
	states := a.snapshotStates(func(st *syncpb.InterfaceState) bool { return st.PodUid == uid })
	for _, st := range states {
		profile, ok := a.cfg.ResolveProfile(st.NetworkProfile)
		if !ok {
			log.Printf("skip release for %s/%s: profile %s not found", st.Namespace, st.PodName, st.NetworkProfile)
			continue
		}
		job := syncJob{
			kind:       jobKindRelease,
			profile:    profile,
			profileKey: st.NetworkProfile,
			relReq: aws.ReleaseRequest{
				PodUID:         st.PodUid,
				PodName:        st.PodName,
				Namespace:      st.Namespace,
				InterfaceName:  st.InterfaceName,
				NetworkProfile: st.NetworkProfile,
			},
		}
		a.enqueueJob(job)
	}
}

func main() {
	metricsAddr := flag.String("metrics-address", ":9090", "address for Prometheus metrics")
	grpcSocket := flag.String("grpc-socket", "/run/multivpc-eni-agent.sock", "unix domain socket for CNI communication")
	queueSize := flag.Int("queue-size", 128, "size of the sync work queue")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "node name for filtering resources")
	kubeconfigPath := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "optional kubeconfig path")
	profilesPath := flag.String("network-profiles", "/etc/multivpc/network-profiles.yaml", "path to logical network profile definitions")
	awsRegion := flag.String("aws-region", os.Getenv("AWS_REGION"), "AWS region")
	flag.Parse()

	if *nodeName == "" {
		log.Fatal("--node-name or NODE_NAME must be set")
	}
	if *awsRegion == "" {
		log.Fatal("--aws-region or AWS_REGION must be set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := syncconfig.Load(*profilesPath)
	if err != nil {
		log.Fatalf("load network profiles: %v", err)
	}

	awsManager, err := aws.NewManager(ctx, *awsRegion)
	if err != nil {
		log.Fatalf("initialise aws manager: %v", err)
	}

	kubeClient, err := buildKubeClient(*kubeconfigPath)
	if err != nil {
		log.Fatalf("initialise kubernetes client: %v", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := newAgentMetrics(registry)
	agent := newAgentServer(awsManager, cfg, *nodeName, metrics, *queueSize)
	agent.start(ctx)

	watcher, err := kube.NewPodWatcher(kubeClient, *nodeName, agent)
	if err != nil {
		log.Fatalf("create pod watcher: %v", err)
	}
	go watcher.Run(ctx)

	if err := ensureSocketDirectory(*grpcSocket); err != nil {
		log.Fatalf("prepare socket: %v", err)
	}
	listener, err := net.Listen("unix", *grpcSocket)
	if err != nil {
		log.Fatalf("listen on unix socket: %v", err)
	}

	grpcServer := grpc.NewServer()
	syncpb.RegisterIpSyncServiceServer(grpcServer, agent)

	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Fatalf("grpc server failed: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricSrv := &http.Server{Addr: *metricsAddr, Handler: mux}

	go func() {
		if err := metricSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down")

	grpcServer.GracefulStop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = metricSrv.Shutdown(shutdownCtx)
}

func ensureSocketDirectory(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return nil
}

func buildKubeClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	var cfg *rest.Config
	var err error
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil && kubeconfigPath != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		}
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func isPodTerminating(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return true
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	default:
		return false
	}
}
