package aws

import (
	"context"
	"testing"

	awstypes "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	syncconfig "github.com/soracom/multus-multivpc-cni/pkg/config"
)

type fakeEC2Client struct {
	describeOutput            *ec2.DescribeNetworkInterfacesOutput
	afterDetachDescribeOutput *ec2.DescribeNetworkInterfacesOutput
	describeInstOut           *ec2.DescribeInstancesOutput
	describeTypesOut          *ec2.DescribeInstanceTypesOutput

	createInput   *ec2.CreateNetworkInterfaceInput
	attachInput   *ec2.AttachNetworkInterfaceInput
	assignPrivate *ec2.AssignPrivateIpAddressesInput
	assignIPv6    *ec2.AssignIpv6AddressesInput
	modifyInput   *ec2.ModifyNetworkInterfaceAttributeInput
	unassignPriv  *ec2.UnassignPrivateIpAddressesInput
	unassignIPv6  *ec2.UnassignIpv6AddressesInput
	detachInput   *ec2.DetachNetworkInterfaceInput
	deleteInput   *ec2.DeleteNetworkInterfaceInput
}

func (f *fakeEC2Client) DescribeNetworkInterfaces(ctx context.Context, params *ec2.DescribeNetworkInterfacesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if f.describeOutput != nil {
		return f.describeOutput, nil
	}
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}

func (f *fakeEC2Client) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeInstOut != nil {
		return f.describeInstOut, nil
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{
			{
				Instances: []types.Instance{
					{
						InstanceId:   awstypes.String("i-123"),
						InstanceType: types.InstanceTypeT3Small,
						NetworkInterfaces: []types.InstanceNetworkInterface{
							{Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: awstypes.Int32(0)}},
						},
					},
				},
			},
		},
	}, nil
}

func (f *fakeEC2Client) DescribeInstanceTypes(ctx context.Context, params *ec2.DescribeInstanceTypesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	if f.describeTypesOut != nil {
		return f.describeTypesOut, nil
	}
	return &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: []types.InstanceTypeInfo{
			{
				InstanceType: types.InstanceTypeT3Small,
				NetworkInfo: &types.NetworkInfo{
					MaximumNetworkInterfaces: awstypes.Int32(4),
				},
			},
		},
	}, nil
}

func (f *fakeEC2Client) CreateNetworkInterface(ctx context.Context, params *ec2.CreateNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.createInput = params
	return &ec2.CreateNetworkInterfaceOutput{
		NetworkInterface: &types.NetworkInterface{NetworkInterfaceId: awstypes.String("eni-123")},
	}, nil
}

func (f *fakeEC2Client) AttachNetworkInterface(ctx context.Context, params *ec2.AttachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.AttachNetworkInterfaceOutput, error) {
	f.attachInput = params
	return &ec2.AttachNetworkInterfaceOutput{AttachmentId: awstypes.String("attach-1")}, nil
}

func (f *fakeEC2Client) ModifyNetworkInterfaceAttribute(ctx context.Context, params *ec2.ModifyNetworkInterfaceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	f.modifyInput = params
	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

func (f *fakeEC2Client) AssignPrivateIpAddresses(ctx context.Context, params *ec2.AssignPrivateIpAddressesInput, optFns ...func(*ec2.Options)) (*ec2.AssignPrivateIpAddressesOutput, error) {
	f.assignPrivate = params
	return &ec2.AssignPrivateIpAddressesOutput{}, nil
}

func (f *fakeEC2Client) AssignIpv6Addresses(ctx context.Context, params *ec2.AssignIpv6AddressesInput, optFns ...func(*ec2.Options)) (*ec2.AssignIpv6AddressesOutput, error) {
	f.assignIPv6 = params
	return &ec2.AssignIpv6AddressesOutput{}, nil
}

func (f *fakeEC2Client) UnassignPrivateIpAddresses(ctx context.Context, params *ec2.UnassignPrivateIpAddressesInput, optFns ...func(*ec2.Options)) (*ec2.UnassignPrivateIpAddressesOutput, error) {
	f.unassignPriv = params
	return &ec2.UnassignPrivateIpAddressesOutput{}, nil
}

func (f *fakeEC2Client) UnassignIpv6Addresses(ctx context.Context, params *ec2.UnassignIpv6AddressesInput, optFns ...func(*ec2.Options)) (*ec2.UnassignIpv6AddressesOutput, error) {
	f.unassignIPv6 = params
	return &ec2.UnassignIpv6AddressesOutput{}, nil
}

func (f *fakeEC2Client) DetachNetworkInterface(ctx context.Context, params *ec2.DetachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DetachNetworkInterfaceOutput, error) {
	f.detachInput = params
	if f.afterDetachDescribeOutput != nil {
		f.describeOutput = f.afterDetachDescribeOutput
	}
	return &ec2.DetachNetworkInterfaceOutput{}, nil
}

func (f *fakeEC2Client) DeleteNetworkInterface(ctx context.Context, params *ec2.DeleteNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.deleteInput = params
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func TestEnsureInterfaceCreatesAndAssigns(t *testing.T) {
	fake := &fakeEC2Client{}
	mgr := &Manager{ec2Client: fake, instanceID: "i-123"}

	profile := syncconfig.NetworkProfile{SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"}, Description: "test"}
	req := SyncRequest{
		PodUID:         "uid-1",
		PodName:        "pod",
		Namespace:      "default",
		InterfaceName:  "net1",
		NetworkProfile: "profile",
		IPv4Addresses:  []string{"172.16.0.10"},
	}

	res, err := mgr.EnsureInterface(context.Background(), profile, req)
	if err != nil {
		t.Fatalf("EnsureInterface returned error: %v", err)
	}
	if res.ENIID != "eni-123" {
		t.Fatalf("unexpected eni id: %s", res.ENIID)
	}

	if fake.createInput == nil || awstypes.ToString(fake.createInput.SubnetId) != "subnet-1" {
		t.Fatalf("expected create network interface to be invoked with subnet-1")
	}
	if fake.attachInput == nil || awstypes.ToString(fake.attachInput.InstanceId) != "i-123" {
		t.Fatalf("expected interface to be attached to instance i-123")
	}
	if fake.attachInput == nil || awstypes.ToInt32(fake.attachInput.DeviceIndex) != 1 {
		t.Fatalf("expected device index 1 to be selected, got %v", fake.attachInput.DeviceIndex)
	}
	if fake.assignPrivate == nil || len(fake.assignPrivate.PrivateIpAddresses) != 1 {
		t.Fatalf("expected private ip assignment to occur")
	}
}

func TestReleaseInterfaceDetachesAndDeletes(t *testing.T) {
	fake := &fakeEC2Client{}
	fake.describeOutput = &ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []types.NetworkInterface{
			{
				NetworkInterfaceId: awstypes.String("eni-123"),
				Attachment:         &types.NetworkInterfaceAttachment{AttachmentId: awstypes.String("attach-1")},
				PrivateIpAddresses: []types.NetworkInterfacePrivateIpAddress{
					{PrivateIpAddress: awstypes.String("172.16.0.10"), Primary: awstypes.Bool(false)},
					{PrivateIpAddress: awstypes.String("172.16.0.11"), Primary: awstypes.Bool(true)},
				},
			},
		},
	}
	fake.afterDetachDescribeOutput = &ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []types.NetworkInterface{
			{
				NetworkInterfaceId: awstypes.String("eni-123"),
				Status:             types.NetworkInterfaceStatusAvailable,
			},
		},
	}

	mgr := &Manager{ec2Client: fake}

	profile := syncconfig.NetworkProfile{}
	req := ReleaseRequest{PodUID: "uid-1"}

	if err := mgr.ReleaseInterface(context.Background(), profile, req); err != nil {
		t.Fatalf("ReleaseInterface returned error: %v", err)
	}

	if fake.unassignPriv == nil || len(fake.unassignPriv.PrivateIpAddresses) != 1 {
		t.Fatalf("expected secondary private ip to be unassigned")
	}
	if fake.detachInput == nil || awstypes.ToString(fake.detachInput.AttachmentId) != "attach-1" {
		t.Fatalf("expected detach to be invoked")
	}
	if fake.deleteInput == nil || awstypes.ToString(fake.deleteInput.NetworkInterfaceId) != "eni-123" {
		t.Fatalf("expected delete to be invoked")
	}
}
