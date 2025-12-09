//go:build !linux

package netns

import (
	"fmt"
	"net"
)

// InterfaceInfo is a minimal stub for non-Linux builds.
type InterfaceInfo struct {
	Name     string
	Hardware net.HardwareAddr
	MTU      int
	IPv4     []net.IP
	IPv6     []net.IP
}

// InspectInterface returns an error on non-Linux platforms.
func InspectInterface(nsPath, ifName string) (*InterfaceInfo, error) {
	return nil, fmt.Errorf("netns inspection is only supported on Linux")
}
