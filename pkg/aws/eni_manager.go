package aws

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	awstypes "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	syncconfig "github.com/soracom/multus-multivpc-cni/pkg/config"
)

// SyncRequest represents the desired ENI state for a Pod interface.
type SyncRequest struct {
	PodUID         string
	PodName        string
	Namespace      string
	InterfaceName  string
	NetworkProfile string
	IPv4Addresses  []string
	IPv6Addresses  []string
	MacAddress     string
}

// ReleaseRequest holds the data required to tear down an ENI association.
type ReleaseRequest struct {
	PodUID         string
	Namespace      string
	PodName        string
	InterfaceName  string
	NetworkProfile string
}

// SyncResult summarises the result of a sync attempt.
type SyncResult struct {
	ENIID        string
	AttachmentID string
	AssignedIPv4 []string
	AssignedIPv6 []string
	MacAddress   string
}

type ec2API interface {
	DescribeNetworkInterfaces(ctx context.Context, params *ec2.DescribeNetworkInterfacesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(ctx context.Context, params *ec2.DescribeInstanceTypesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	CreateNetworkInterface(ctx context.Context, params *ec2.CreateNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error)
	AttachNetworkInterface(ctx context.Context, params *ec2.AttachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.AttachNetworkInterfaceOutput, error)
	ModifyNetworkInterfaceAttribute(ctx context.Context, params *ec2.ModifyNetworkInterfaceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
	AssignPrivateIpAddresses(ctx context.Context, params *ec2.AssignPrivateIpAddressesInput, optFns ...func(*ec2.Options)) (*ec2.AssignPrivateIpAddressesOutput, error)
	AssignIpv6Addresses(ctx context.Context, params *ec2.AssignIpv6AddressesInput, optFns ...func(*ec2.Options)) (*ec2.AssignIpv6AddressesOutput, error)
	UnassignPrivateIpAddresses(ctx context.Context, params *ec2.UnassignPrivateIpAddressesInput, optFns ...func(*ec2.Options)) (*ec2.UnassignPrivateIpAddressesOutput, error)
	UnassignIpv6Addresses(ctx context.Context, params *ec2.UnassignIpv6AddressesInput, optFns ...func(*ec2.Options)) (*ec2.UnassignIpv6AddressesOutput, error)
	DetachNetworkInterface(ctx context.Context, params *ec2.DetachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DetachNetworkInterfaceOutput, error)
	DeleteNetworkInterface(ctx context.Context, params *ec2.DeleteNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error)
}

// Manager orchestrates AWS API operations for ENI lifecycle.
type Manager struct {
	ec2Client ec2API
	imds      *imds.Client

	instanceID       string
	availabilityZone string
	mutex            sync.RWMutex
}

// NewManager constructs a Manager with retry configuration and IMDS metadata discovery.
func NewManager(ctx context.Context, region string) (*Manager, error) {
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(region),
		awscfg.WithRetryer(func() awstypes.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 6
				o.MaxBackoff = 10 * time.Second
			})
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	mgr := &Manager{
		ec2Client: ec2.NewFromConfig(cfg),
		imds:      imds.NewFromConfig(cfg),
	}
	if err := mgr.refreshMetadata(ctx); err != nil {
		return nil, err
	}
	return mgr, nil
}

// refreshMetadata loads the local EC2 instance identity from IMDSv2.
func (m *Manager) refreshMetadata(ctx context.Context) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	doc, err := m.imds.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return fmt.Errorf("fetch instance identity: %w", err)
	}
	m.instanceID = doc.InstanceID
	m.availabilityZone = doc.AvailabilityZone
	return nil
}

// EnsureInterface ensures that a Pod interface is backed by an ENI with the requested addresses.
func (m *Manager) EnsureInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req SyncRequest) (*SyncResult, error) {
	if cfg.SubnetID == "" {
		return nil, fmt.Errorf("network profile %q missing subnetId", req.NetworkProfile)
	}
	if len(cfg.SecurityGroupIDs) == 0 {
		return nil, fmt.Errorf("network profile %q missing securityGroupIds", req.NetworkProfile)
	}

	attachmentTag := m.buildAttachmentTag(req.PodUID)

	eni, attachment, err := m.findExistingInterface(ctx, attachmentTag)
	if err != nil {
		return nil, err
	}

	if eni == nil {
		eni, attachment, err = m.createAndAttachInterface(ctx, cfg, attachmentTag)
		if err != nil {
			return nil, err
		}
	}

	if err := m.assignIPAddresses(ctx, eni, req); err != nil {
		return nil, err
	}

	return &SyncResult{
		ENIID:        awstypes.ToString(eni.NetworkInterfaceId),
		AttachmentID: awstypes.ToString(attachment.AttachmentId),
		AssignedIPv4: req.IPv4Addresses,
		AssignedIPv6: req.IPv6Addresses,
		MacAddress:   awstypes.ToString(eni.MacAddress),
	}, nil
}

// ReleaseInterface unassigns IPs and optionally detaches and deletes the ENI.
func (m *Manager) ReleaseInterface(ctx context.Context, cfg syncconfig.NetworkProfile, req ReleaseRequest) error {
	attachmentTag := m.buildAttachmentTag(req.PodUID)
	eni, attachment, err := m.findExistingInterface(ctx, attachmentTag)
	if err != nil {
		return err
	}
	if eni == nil {
		return nil
	}

	if err := m.unassignIPAddresses(ctx, eni); err != nil {
		return err
	}

	if err := m.detachInterface(ctx, attachment); err != nil {
		return err
	}
	if err := m.waitForDetachment(ctx, eni); err != nil {
		return err
	}

	return m.deleteInterface(ctx, eni)
}

func (m *Manager) buildAttachmentTag(podUID string) string {
	return fmt.Sprintf("multivpc-%s", podUID)
}

func (m *Manager) nextAvailableDeviceIndex(ctx context.Context) (int32, error) {
	instOut, err := m.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{m.instanceID},
	})
	if err != nil {
		return 0, fmt.Errorf("describe instance %s: %w", m.instanceID, err)
	}
	if len(instOut.Reservations) == 0 || len(instOut.Reservations[0].Instances) == 0 {
		return 0, fmt.Errorf("instance %s not found in describe response", m.instanceID)
	}

	instance := instOut.Reservations[0].Instances[0]

	used := make(map[int32]struct{})
	for _, iface := range instance.NetworkInterfaces {
		if iface.Attachment != nil && iface.Attachment.DeviceIndex != nil {
			used[*iface.Attachment.DeviceIndex] = struct{}{}
		}
	}

	maxInterfaces := int32(15) // sensible default for Nitro-based nodes
	if instance.InstanceType != "" {
		typeOut, err := m.ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
			InstanceTypes: []types.InstanceType{instance.InstanceType},
		})
		if err != nil {
			log.Printf("describe instance types failed (falling back to default max interfaces): %v", err)
		} else if len(typeOut.InstanceTypes) > 0 {
			if ni := typeOut.InstanceTypes[0].NetworkInfo; ni != nil && ni.MaximumNetworkInterfaces != nil {
				maxInterfaces = *ni.MaximumNetworkInterfaces
			}
		}
	}

	if maxInterfaces < 2 {
		maxInterfaces = 2
	}

	for idx := int32(1); idx < maxInterfaces; idx++ {
		if _, ok := used[idx]; !ok {
			return idx, nil
		}
	}

	return 0, fmt.Errorf("no available device index on instance %s (used=%v, max=%d)", m.instanceID, used, maxInterfaces)
}

func (m *Manager) findExistingInterface(ctx context.Context, attachmentTag string) (*types.NetworkInterface, *types.NetworkInterfaceAttachment, error) {
	input := &ec2.DescribeNetworkInterfacesInput{
		Filters: []types.Filter{
			{
				Name:   awstypes.String("tag:MultusSyncAttachment"),
				Values: []string{attachmentTag},
			},
			{
				Name:   awstypes.String("attachment.instance-id"),
				Values: []string{m.instanceID},
			},
		},
	}

	out, err := m.ec2Client.DescribeNetworkInterfaces(ctx, input)
	if err != nil {
		return nil, nil, fmt.Errorf("describe network interfaces: %w", err)
	}
	if len(out.NetworkInterfaces) == 0 {
		return nil, nil, nil
	}
	eni := out.NetworkInterfaces[0]
	if eni.Attachment == nil {
		return nil, nil, fmt.Errorf("eni %s missing attachment", awstypes.ToString(eni.NetworkInterfaceId))
	}
	return &eni, eni.Attachment, nil
}

func (m *Manager) createAndAttachInterface(ctx context.Context, cfg syncconfig.NetworkProfile, attachmentTag string) (*types.NetworkInterface, *types.NetworkInterfaceAttachment, error) {
	deviceIndex, err := m.nextAvailableDeviceIndex(ctx)
	if err != nil {
		return nil, nil, err
	}

	createOut, err := m.ec2Client.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    awstypes.String(cfg.SubnetID),
		Groups:      cfg.SecurityGroupIDs,
		Description: awstypes.String(cfg.Description),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeNetworkInterface,
				Tags: []types.Tag{
					{Key: awstypes.String("MultusSyncAttachment"), Value: awstypes.String(attachmentTag)},
				},
			},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create network interface: %w", err)
	}

	attachOut, err := m.ec2Client.AttachNetworkInterface(ctx, &ec2.AttachNetworkInterfaceInput{
		DeviceIndex:        awstypes.Int32(deviceIndex),
		InstanceId:         awstypes.String(m.instanceID),
		NetworkInterfaceId: createOut.NetworkInterface.NetworkInterfaceId,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("attach network interface: %w", err)
	}

	// Enable delete on termination.
	_, err = m.ec2Client.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
		Attachment: &types.NetworkInterfaceAttachmentChanges{
			AttachmentId:        attachOut.AttachmentId,
			DeleteOnTermination: awstypes.Bool(true),
		},
		NetworkInterfaceId: createOut.NetworkInterface.NetworkInterfaceId,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("modify attachment: %w", err)
	}

	log.Printf("created and attached eni %s", awstypes.ToString(createOut.NetworkInterface.NetworkInterfaceId))

	return createOut.NetworkInterface, &types.NetworkInterfaceAttachment{
		AttachmentId: attachOut.AttachmentId,
	}, nil
}

func (m *Manager) assignIPAddresses(ctx context.Context, eni *types.NetworkInterface, req SyncRequest) error {
	if len(req.IPv4Addresses) > 0 {
		existing := map[string]struct{}{}
		for _, addr := range eni.PrivateIpAddresses {
			if addr.PrivateIpAddress != nil {
				existing[awstypes.ToString(addr.PrivateIpAddress)] = struct{}{}
			}
		}
		var toAssign []string
		for _, ip := range req.IPv4Addresses {
			if _, ok := existing[ip]; !ok {
				toAssign = append(toAssign, ip)
			}
		}
		if len(toAssign) > 0 {
			if _, err := m.ec2Client.AssignPrivateIpAddresses(ctx, &ec2.AssignPrivateIpAddressesInput{
				NetworkInterfaceId: eni.NetworkInterfaceId,
				PrivateIpAddresses: toAssign,
			}); err != nil {
				return fmt.Errorf("assign ipv4 addresses: %w", err)
			}
		}
	}
	if len(req.IPv6Addresses) > 0 {
		existing := map[string]struct{}{}
		for _, addr := range eni.Ipv6Addresses {
			if addr.Ipv6Address != nil {
				existing[awstypes.ToString(addr.Ipv6Address)] = struct{}{}
			}
		}
		var toAssign []string
		for _, ip := range req.IPv6Addresses {
			if _, ok := existing[ip]; !ok {
				toAssign = append(toAssign, ip)
			}
		}
		if len(toAssign) > 0 {
			if _, err := m.ec2Client.AssignIpv6Addresses(ctx, &ec2.AssignIpv6AddressesInput{
				NetworkInterfaceId: eni.NetworkInterfaceId,
				Ipv6Addresses:      toAssign,
			}); err != nil {
				return fmt.Errorf("assign ipv6 addresses: %w", err)
			}
		}
	}
	return nil
}

func (m *Manager) unassignIPAddresses(ctx context.Context, eni *types.NetworkInterface) error {
	var ipv4 []string
	for _, addr := range eni.PrivateIpAddresses {
		if awstypes.ToBool(addr.Primary) {
			continue
		}
		ipv4 = append(ipv4, awstypes.ToString(addr.PrivateIpAddress))
	}
	if len(ipv4) > 0 {
		if _, err := m.ec2Client.UnassignPrivateIpAddresses(ctx, &ec2.UnassignPrivateIpAddressesInput{
			NetworkInterfaceId: eni.NetworkInterfaceId,
			PrivateIpAddresses: ipv4,
		}); err != nil {
			return fmt.Errorf("unassign ipv4: %w", err)
		}
	}
	if len(eni.Ipv6Addresses) > 0 {
		addrs := make([]string, 0, len(eni.Ipv6Addresses))
		for _, addr := range eni.Ipv6Addresses {
			addrs = append(addrs, awstypes.ToString(addr.Ipv6Address))
		}
		if _, err := m.ec2Client.UnassignIpv6Addresses(ctx, &ec2.UnassignIpv6AddressesInput{
			NetworkInterfaceId: eni.NetworkInterfaceId,
			Ipv6Addresses:      addrs,
		}); err != nil {
			return fmt.Errorf("unassign ipv6: %w", err)
		}
	}
	return nil
}

func (m *Manager) detachInterface(ctx context.Context, attachment *types.NetworkInterfaceAttachment) error {
	if attachment == nil || attachment.AttachmentId == nil {
		return nil
	}
	_, err := m.ec2Client.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
		AttachmentId: attachment.AttachmentId,
		Force:        awstypes.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("detach network interface: %w", err)
	}
	return nil
}

func (m *Manager) deleteInterface(ctx context.Context, eni *types.NetworkInterface) error {
	if eni == nil || eni.NetworkInterfaceId == nil {
		return nil
	}
	_, err := m.ec2Client.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: eni.NetworkInterfaceId,
	})
	if err != nil {
		return fmt.Errorf("delete network interface: %w", err)
	}
	return nil
}

func (m *Manager) waitForDetachment(ctx context.Context, eni *types.NetworkInterface) error {
	if eni == nil || eni.NetworkInterfaceId == nil {
		return nil
	}
	// EC2 detachment is asynchronous; poll until the ENI is no longer attached before deleting.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for eni %s to detach", awstypes.ToString(eni.NetworkInterfaceId))
		}

		out, err := m.ec2Client.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{awstypes.ToString(eni.NetworkInterfaceId)},
		})
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if len(out.NetworkInterfaces) == 0 {
			return nil
		}
		current := out.NetworkInterfaces[0]
		if current.Attachment == nil || current.Status == types.NetworkInterfaceStatusAvailable {
			return nil
		}

		time.Sleep(2 * time.Second)
	}
}

// DescribeState provides a human readable summary for Prometheus or debugging.
func (m *Manager) DescribeState(req SyncRequest, res *SyncResult) string {
	var sb strings.Builder
	sb.WriteString(req.PodName)
	sb.WriteString("/")
	sb.WriteString(req.Namespace)
	sb.WriteString(" -> ")
	if res != nil {
		sb.WriteString(res.ENIID)
	}
	return sb.String()
}
