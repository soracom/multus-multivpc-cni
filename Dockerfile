FROM golang:1.25 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0

RUN GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -o /out/multivpc-eni-agent ./cmd/multivpc-eni-agent \
 && GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -o /out/multus-multivpc-cni ./cmd/multus-multivpc-cni \
 && GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -o /out/multivpc-taint-controller ./cmd/multivpc-taint-controller

FROM alpine:3.23

# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates iproute2 ethtool \
 && mkdir -p /etc/multivpc

COPY --from=builder /out/multivpc-eni-agent /usr/local/bin/multivpc-eni-agent
COPY --from=builder /out/multus-multivpc-cni /usr/local/bin/multus-multivpc-cni
COPY --from=builder /out/multivpc-taint-controller /usr/local/bin/multivpc-taint-controller

ENTRYPOINT ["/usr/local/bin/multivpc-eni-agent"]
