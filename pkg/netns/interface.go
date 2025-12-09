//go:build linux

package netns

import (
	"fmt"
	"net"

	nslib "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// InterfaceInfo captures network namespace interface attributes.
type InterfaceInfo struct {
	Name     string
	Hardware net.HardwareAddr
	MTU      int
	IPv4     []net.IP
	IPv6     []net.IP
}

// InspectInterface collects interface metadata from the provided network namespace path.
func InspectInterface(nsPath, ifName string) (*InterfaceInfo, error) {
	if nsPath == "" {
		return nil, fmt.Errorf("nsPath must not be empty")
	}
	if ifName == "" {
		return nil, fmt.Errorf("interface name must not be empty")
	}

	nsHandle, err := nslib.GetNS(nsPath)
	if err != nil {
		return nil, fmt.Errorf("open netns %s: %w", nsPath, err)
	}
	defer func() {
		_ = nsHandle.Close()
	}()

	info := &InterfaceInfo{Name: ifName}

	err = nsHandle.Do(func(_ nslib.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("find link %s: %w", ifName, err)
		}
		info.Hardware = link.Attrs().HardwareAddr
		info.MTU = link.Attrs().MTU

		addrList, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("addr list: %w", err)
		}
		for _, addr := range addrList {
			if addr.IP == nil {
				continue
			}
			if addr.IP.To4() != nil {
				info.IPv4 = append(info.IPv4, addr.IP)
			} else {
				info.IPv6 = append(info.IPv6, addr.IP)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return info, nil
}
