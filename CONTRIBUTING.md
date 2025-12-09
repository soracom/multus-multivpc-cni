# Contribution Guideline

For Kubernetes developers.

### Table of Contents

- [Development Workflow](#development-workflow)
  - [Prerequisites](#prerequisites)
  - [Pre-PR Checklist](#pre-pr-checklist)
  - [Directory Highlights](#directory-highlights)
- [Building Container Image](#building-container-image)

## Development Workflow

### Prerequisites

- Linux worker nodes (or local namespaces) with Multus and the Amazon VPC CNI installed.
- Protocol Buffers tooling (`protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`) available in `$GOBIN`.

### Pre-PR Checklist

Run the same commands as CI before opening a pull request.

```shellsession
$ test -z "$(gofmt -l .)" || gofmt -w $(gofmt -l .)
$ go test ./... -cover
$ go vet ./...
$ staticcheck ./...
$ golangci-lint run ./...
$ helm lint charts/multus-multivpc-cni
```

Skip only if Dockerfile was not touched.

```shellsession
$ hadolint Dockerfile
$ docker run --rm -i hadolint/hadolint < Dockerfile  # or via container
```

Regenerate protobufs when the schema changes:

```shellsession
$ protoc --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. proto/syncpb/ip_sync.proto
```

### Directory Highlights

- [`cmd/multus-multivpc-cni`](cmd/multus-multivpc-cni): CNI delegate entry point that handles `CmdAdd`/`CmdDel`, inspects the Pod netns for interface data, and issues `ReportInterface` calls.
- [`cmd/multivpc-eni-agent`](cmd/multivpc-eni-agent): Node-resident agent that consumes informer events and gRPC requests, serialises AWS calls, and publishes Prometheus metrics.
- [`cmd/multivpc-taint-controller`](cmd/multivpc-taint-controller): Optional controller that manages a startup taint per node based on agent Pod readiness, keeping Multus workloads off nodes until the ENI agent is ready.
- [`pkg/aws`](pkg/aws): ENI lifecycle management built on AWS SDK v2. `Manager` exposes `EnsureInterface` and `ReleaseInterface`, with fake clients covering unit tests.
- [`pkg/config`](pkg/config): Loader for network profile YAML delivered via Helm. Resolves subnets, security groups, and optional IPv6 configuration.
- [`pkg/kube`](pkg/kube): Pod informer helper constrained to the local node via `PodEventHandler`.
- [`pkg/netns`](pkg/netns): Linux namespace utilities wrapping `ns` and `netlink`; includes build-tagged stubs for non-Linux targets.
- [`proto/syncpb`](proto/syncpb): Source `.proto` and generated `syncpb` Go code that formalise the CNI↔agent contract.
- [`charts/multus-multivpc-cni`](charts/multus-multivpc-cni): Helm chart containing the DaemonSet, ConfigMap, RBAC, and values surfaced to end users.

## Building Container Image

This repository ships a multi-stage `Dockerfile` that produces both the CNI binary and the node agent.
Use Buildx to build and push a multi-architecture image (linux/amd64 and linux/arm64) — the same configuration used by CI/CD.

```bash
# Lint the Dockerfile before building
docker run --rm -i hadolint/hadolint < Dockerfile

# Build and push a multi-arch image
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t 123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/multus-multivpc-cni:dev \
  --push .
```

If Buildx is not available, you can still build a single-architecture image with `docker build`, but a dual-architecture build ensures the image runs on both Graviton and x86 worker nodes.
