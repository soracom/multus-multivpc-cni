package main

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/soracom/multus-multivpc-cni/pkg/aws"
	syncconfig "github.com/soracom/multus-multivpc-cni/pkg/config"
	syncpb "github.com/soracom/multus-multivpc-cni/proto/syncpb"
)

type recordedEnsure struct {
	cfg syncconfig.NetworkProfile
	req aws.SyncRequest
}

type recordedRelease struct {
	cfg syncconfig.NetworkProfile
	req aws.ReleaseRequest
}

type fakeManager struct {
	ensureResult *aws.SyncResult
	ensureErr    error
	releaseErr   error

	ensures  []recordedEnsure
	releases []recordedRelease
}

func (f *fakeManager) EnsureInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req aws.SyncRequest) (*aws.SyncResult, error) {
	f.ensures = append(f.ensures, recordedEnsure{cfg: cfg, req: req})
	return f.ensureResult, f.ensureErr
}

func (f *fakeManager) ReleaseInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req aws.ReleaseRequest) error {
	f.releases = append(f.releases, recordedRelease{cfg: cfg, req: req})
	return f.releaseErr
}

func drainJob(t *testing.T, srv *agentServer) syncJob {
	select {
	case job := <-srv.queue:
		srv.metrics.queueDepth.Dec()
		return job
	case <-time.After(time.Second):
		t.Fatal("expected job to be queued")
	}
	return syncJob{}
}

func TestAgentReportFlowQueuesEnsureJob(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := newAgentMetrics(reg)

	cfg := &syncconfig.Config{NetworkProfiles: map[string]syncconfig.NetworkProfile{
		"profile-a": {
			SubnetID:         "subnet-123",
			SecurityGroupIDs: []string{"sg-1"},
			Description:      "test",
		},
	}}

	mgr := &fakeManager{ensureResult: &aws.SyncResult{ENIID: "eni-1", AssignedIPv4: []string{"172.16.0.10"}}}
	srv := newAgentServer(mgr, cfg, "ip-10-0-0-1", metrics, 4)

	resp, err := srv.ReportInterface(ctx, &syncpb.ReportRequest{
		NodeName:       "ip-10-0-0-1",
		Namespace:      "default",
		PodName:        "pod-a",
		PodUid:         "uid-1",
		InterfaceName:  "net1",
		NetworkProfile: "profile-a",
		Ipv4Addresses:  []string{"172.16.0.10"},
	})
	if err != nil {
		t.Fatalf("ReportInterface returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected request to be accepted")
	}

	job := drainJob(t, srv)
	srv.handleJob(ctx, job)

	if len(mgr.ensures) != 1 {
		t.Fatalf("expected ensure to be called once, got %d", len(mgr.ensures))
	}
	call := mgr.ensures[0]
	if call.cfg.SubnetID != "subnet-123" {
		t.Fatalf("expected subnet subnet-123, got %s", call.cfg.SubnetID)
	}
	if len(call.req.IPv4Addresses) != 1 || call.req.IPv4Addresses[0] != "172.16.0.10" {
		t.Fatalf("unexpected IPv4 addresses: %#v", call.req.IPv4Addresses)
	}

	states := srv.snapshotStates(func(st *syncpb.InterfaceState) bool { return st.PodUid == "uid-1" })
	if len(states) != 1 {
		t.Fatalf("expected one state entry, got %d", len(states))
	}
	if states[0].EniId != "eni-1" {
		t.Fatalf("state did not record ENI assignment: %#v", states[0])
	}
}

func TestAgentReleaseFlowQueuesReleaseJob(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := newAgentMetrics(reg)

	cfg := &syncconfig.Config{NetworkProfiles: map[string]syncconfig.NetworkProfile{
		"profile-a": {
			SubnetID:         "subnet-123",
			SecurityGroupIDs: []string{"sg-1"},
			Description:      "test",
		},
	}}

	mgr := &fakeManager{ensureResult: &aws.SyncResult{ENIID: "eni-1"}}
	srv := newAgentServer(mgr, cfg, "ip-10-0-0-1", metrics, 4)

	if _, err := srv.ReportInterface(ctx, &syncpb.ReportRequest{
		NodeName:       "ip-10-0-0-1",
		Namespace:      "default",
		PodName:        "pod-a",
		PodUid:         "uid-1",
		InterfaceName:  "net1",
		NetworkProfile: "profile-a",
		Ipv4Addresses:  []string{"172.16.0.10"},
	}); err != nil {
		t.Fatalf("ReportInterface failed: %v", err)
	}
	job := drainJob(t, srv)
	srv.handleJob(ctx, job)

	relResp, err := srv.ReleaseInterface(ctx, &syncpb.ReleaseRequest{
		Namespace:      "default",
		PodName:        "pod-a",
		PodUid:         "uid-1",
		InterfaceName:  "net1",
		NetworkProfile: "profile-a",
	})
	if err != nil {
		t.Fatalf("ReleaseInterface failed: %v", err)
	}
	if !relResp.GetAccepted() {
		t.Fatalf("expected release to be accepted")
	}

	releaseJob := drainJob(t, srv)
	srv.handleJob(ctx, releaseJob)

	if len(mgr.releases) != 1 {
		t.Fatalf("expected release call, got %d", len(mgr.releases))
	}
	if len(srv.snapshotStates(func(st *syncpb.InterfaceState) bool { return true })) != 0 {
		t.Fatalf("expected state to be cleared after release")
	}
}

func TestAgentRejectsUnknownProfile(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := newAgentMetrics(reg)
	cfg := &syncconfig.Config{NetworkProfiles: map[string]syncconfig.NetworkProfile{}}
	mgr := &fakeManager{ensureResult: &aws.SyncResult{}}
	srv := newAgentServer(mgr, cfg, "node", metrics, 2)

	_, err := srv.ReportInterface(ctx, &syncpb.ReportRequest{
		NodeName:       "node",
		Namespace:      "default",
		PodName:        "pod",
		PodUid:         "uid",
		InterfaceName:  "net1",
		NetworkProfile: "missing",
	})
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
}
