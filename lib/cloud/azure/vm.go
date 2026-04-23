/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package azure

import (
	"context"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
)

// virtualScaleSetUniformVMResourceType represents the resource type of uniform
// virtual scale set VMs.
const virtualScaleSetUniformVMResourceType = "virtualMachineScaleSets/virtualMachines"

// armCompute provides an interface for an Azure virtual machine client.
type armCompute interface {
	// Get retrieves information about an Azure virtual machine.
	Get(ctx context.Context, resourceGroupName string, vmName string, options *armcompute.VirtualMachinesClientGetOptions) (armcompute.VirtualMachinesClientGetResponse, error)
	// NewListPager lists Azure virtual Machines.
	NewListPager(resourceGroup string, opts *armcompute.VirtualMachinesClientListOptions) *runtime.Pager[armcompute.VirtualMachinesClientListResponse]
	// NewListAllPager lists Azure virtual machines in any resource group.
	NewListAllPager(opts *armcompute.VirtualMachinesClientListAllOptions) *runtime.Pager[armcompute.VirtualMachinesClientListAllResponse]
}

// scaleSet provides an interfaces for an Azure VM scale set client.
type scaleSet interface {
	// Get retrieves a virtual machine from a VM scale set.
	Get(ctx context.Context, resourceGroupName string, vmScaleSetName string, instanceID string, options *armcompute.VirtualMachineScaleSetVMsClientGetOptions) (armcompute.VirtualMachineScaleSetVMsClientGetResponse, error)
}

// PowerState represents the power state of an Azure virtual machine.
type PowerState string

const (
	// PowerStateUnknown indicates that no PowerState/* status entry was present.
	PowerStateUnknown PowerState = ""
	// PowerStateRunning indicates the VM is running.
	PowerStateRunning PowerState = "running"
	// PowerStateDeallocated indicates the VM is deallocated.
	PowerStateDeallocated PowerState = "deallocated"
	// PowerStateStopped indicates the VM is stopped but still allocated.
	PowerStateStopped PowerState = "stopped"
	// PowerStateOther indicates that a PowerState/* status entry was present,
	// but its suffix represents an unknown or unsupported power state.
	PowerStateOther PowerState = "other"
)

// ParsePowerState extracts the first PowerState/* status from an InstanceView status list.
//
// It returns PowerStateRunning for PowerState/running, PowerStateDeallocated for
// PowerState/deallocated, and PowerStateStopped for PowerState/stopped.
// PowerStateOther means a PowerState/* entry exists but its suffix is unsupported
// (for example, PowerState/starting). PowerStateUnknown means no PowerState/*
// entry exists at all.
func ParsePowerState(statuses []*armcompute.InstanceViewStatus) PowerState {
	for _, status := range statuses {
		if status == nil || status.Code == nil {
			continue
		}

		suffix, ok := strings.CutPrefix(*status.Code, "PowerState/")
		if !ok {
			continue
		}

		switch suffix {
		case "running":
			return PowerStateRunning
		case "deallocated":
			return PowerStateDeallocated
		case "stopped":
			return PowerStateStopped
		default:
			return PowerStateOther
		}
	}

	return PowerStateUnknown
}

// SkippedVM pairs a VM that was filtered out with its detected OS
// type, so callers can log exactly why the VM was excluded.
type SkippedVM struct {
	VM     *armcompute.VirtualMachine
	OSType string
}

// FilteredVMs holds the results of partitioning Azure VMs by OS.
type FilteredVMs struct {
	// Linux contains VMs explicitly identified as Linux, plus VMs with unknown OS type
	// (allowed through to avoid silently dropping misconfigured Linux VMs).
	Linux []*armcompute.VirtualMachine
	// Skipped contains VMs with a known non-Linux OS type (e.g. Windows).
	Skipped []SkippedVM
}

// FilterLinuxVMs ignores nil entries and partitions the remaining VMs into
// Linux-compatible (Linux + unknown OS) and skipped (known non-Linux OS like Windows).
// VMs with unknown OS type are allowed through because missing metadata should not
// silently prevent discovery of legitimate Linux VMs.
func FilterLinuxVMs(vms []*armcompute.VirtualMachine) FilteredVMs {
	var result FilteredVMs
	for _, vm := range vms {
		if vm == nil {
			continue
		}
		osType := vmOSType(vm)
		if osType != "" && osType != string(armcompute.OperatingSystemTypesLinux) {
			result.Skipped = append(result.Skipped, SkippedVM{
				VM:     vm,
				OSType: osType,
			})
			continue
		}
		result.Linux = append(result.Linux, vm)
	}

	return result
}

// vmOSType nil-safely extracts the OS type string from a VM.
// Returns empty string if any pointer in the chain is nil.
func vmOSType(vm *armcompute.VirtualMachine) string {
	if vm == nil ||
		vm.Properties == nil ||
		vm.Properties.StorageProfile == nil ||
		vm.Properties.StorageProfile.OSDisk == nil ||
		vm.Properties.StorageProfile.OSDisk.OSType == nil {
		return ""
	}
	return string(*vm.Properties.StorageProfile.OSDisk.OSType)
}

// VMID nil-safely extracts the VM ID (Properties.VMID) from a VM.
// Returns empty string if any pointer in the chain is nil.
func VMID(vm *armcompute.VirtualMachine) string {
	if vm == nil || vm.Properties == nil || vm.Properties.VMID == nil {
		return ""
	}
	return *vm.Properties.VMID
}

// VirtualMachinesClient is a client for Azure virtual machines.
type VirtualMachinesClient interface {
	// Get returns the virtual machine (including scale set VMs) for the given
	// resource ID.
	Get(ctx context.Context, resourceID string) (*VirtualMachine, error)
	// GetByVMID returns the virtual machine for a given VM ID.
	GetByVMID(ctx context.Context, vmID string) (*VirtualMachine, error)
	// ListVirtualMachines gets all of the virtual machines in the given resource group.
	// If resourceGroup is "*", it lists all virtual machines in any resource group.
	ListVirtualMachines(ctx context.Context, resourceGroup string) ([]*armcompute.VirtualMachine, error)
	// ListNonRunningVirtualMachineStates returns known non-running VM power states keyed by
	// resource ID. Uses StatusOnly=true on a subscription-wide ListAll. Omits VMs that
	// cannot be reconciled by vm.ID, as well as VMs whose power state is running or
	// indeterminate.
	ListNonRunningVirtualMachineStates(ctx context.Context) (map[string]PowerState, error)
}

// VirtualMachine represents an Azure virtual machine.
type VirtualMachine struct {
	// ID resource ID.
	ID string `json:"id,omitempty"`
	// Name resource name.
	Name string `json:"name,omitempty"`
	// Subscription is the Azure subscription the VM is in.
	Subscription string
	// ResourceGroup is the resource group the VM is in.
	ResourceGroup string
	// VMID is the VM's ID.
	VMID string
	// Identities are the identities associated with the resource.
	Identities []Identity
}

// Identity represents an Azure virtual machine identity.
type Identity struct {
	// ResourceID the identity resource ID.
	ResourceID string
}

type vmClient struct {
	// api is the Azure virtual machine client.
	api armCompute
	// scaleSetAPI is the Azure VM scale set client.
	scaleSetAPI scaleSet
}

// NewVirtualMachinesClient creates a new Azure virtual machines client by
// subscription and credentials.
func NewVirtualMachinesClient(subscription string, cred azcore.TokenCredential, options *arm.ClientOptions) (VirtualMachinesClient, error) {
	computeAPI, err := armcompute.NewVirtualMachinesClient(subscription, cred, options)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	scaleSetAPI, err := armcompute.NewVirtualMachineScaleSetVMsClient(subscription, cred, options)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return NewVirtualMachinesClientByAPI(computeAPI, scaleSetAPI), nil
}

// NewVirtualMachinesClientByAPI creates a new Azure virtual machines client by
// ARM API client.
func NewVirtualMachinesClientByAPI(api armCompute, scaleSetAPI scaleSet) VirtualMachinesClient {
	return &vmClient{
		api:         api,
		scaleSetAPI: scaleSetAPI,
	}
}

type vmTypes interface {
	*armcompute.VirtualMachine | *armcompute.VirtualMachineScaleSetVM
}

func parseVirtualMachine[T vmTypes](vm T) (*VirtualMachine, error) {
	var (
		id       string
		name     string
		identity *armcompute.VirtualMachineIdentity
		vmID     *string
	)

	switch v := any(vm).(type) {
	case *armcompute.VirtualMachine:
		id = *v.ID
		name = *v.Name
		identity = v.Identity
		if v.Properties != nil {
			vmID = v.Properties.VMID
		}

	case *armcompute.VirtualMachineScaleSetVM:
		id = *v.ID
		name = *v.Name
		identity = v.Identity
		if v.Properties != nil {
			vmID = v.Properties.VMID
		}
	}

	resourceID, err := arm.ParseResourceID(id)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var identities []Identity
	if identity != nil {
		if systemAssigned := StringVal(identity.PrincipalID); systemAssigned != "" {
			identities = append(identities, Identity{ResourceID: systemAssigned})
		}

		for identityID := range identity.UserAssignedIdentities {
			identities = append(identities, Identity{ResourceID: identityID})
		}
	}

	return &VirtualMachine{
		ID:            id,
		Name:          name,
		Subscription:  resourceID.SubscriptionID,
		ResourceGroup: resourceID.ResourceGroupName,
		VMID:          StringVal(vmID),
		Identities:    identities,
	}, nil
}

// Get returns the virtual machine (including scale set VMs) for the given
// resource ID.
func (c *vmClient) Get(ctx context.Context, resourceID string) (*VirtualMachine, error) {
	// Uniform VMSS VMs must be fetched via the scale-set client. Their resource ID format isn't
	// resolvable by the standard VM API. Flexible VMSS VMs use the regular format and fall through.
	parsedResourceID, err := arm.ParseResourceID(resourceID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if parsedResourceID.ResourceType.Type == virtualScaleSetUniformVMResourceType {
		return c.getScaleSetVM(ctx, parsedResourceID)
	}

	resp, err := c.api.Get(ctx, parsedResourceID.ResourceGroupName, parsedResourceID.Name, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	vm, err := parseVirtualMachine(&resp.VirtualMachine)
	return vm, trace.Wrap(err)
}

// GetByVMID returns the virtual machine for a given VM ID.
func (c *vmClient) GetByVMID(ctx context.Context, vmID string) (*VirtualMachine, error) {
	pager := newListAllPager(c.api.NewListAllPager(&armcompute.VirtualMachinesClientListAllOptions{}))
	for pager.more() {
		res, err := pager.nextPage(ctx)
		if err != nil {
			return nil, trace.Wrap(ConvertResponseError(err))
		}

		for _, vm := range res {
			if vm.Properties != nil && *vm.Properties.VMID == vmID {
				result, err := parseVirtualMachine(vm)
				return result, trace.Wrap(err)
			}
		}
	}
	return nil, trace.NotFound("no VM with ID %q", vmID)
}

func (c *vmClient) getScaleSetVM(ctx context.Context, resourceID *arm.ResourceID) (*VirtualMachine, error) {
	if resourceID.Parent == nil {
		return nil, trace.BadParameter("expected resource ID to include scale set as parent resource")
	}

	resp, err := c.scaleSetAPI.Get(ctx, resourceID.ResourceGroupName, resourceID.Parent.Name, resourceID.Name, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	result, err := parseVirtualMachine(&resp.VirtualMachineScaleSetVM)
	return result, trace.Wrap(err)
}

type vmPager struct {
	more     func() bool
	nextPage func(context.Context) ([]*armcompute.VirtualMachine, error)
}

func newListPager(azurePager *runtime.Pager[armcompute.VirtualMachinesClientListResponse]) vmPager {
	return vmPager{
		more: azurePager.More,
		nextPage: func(ctx context.Context) ([]*armcompute.VirtualMachine, error) {
			res, err := azurePager.NextPage(ctx)
			return res.Value, trace.Wrap(err)
		},
	}
}

func newListAllPager(azurePager *runtime.Pager[armcompute.VirtualMachinesClientListAllResponse]) vmPager {
	return vmPager{
		more: azurePager.More,
		nextPage: func(ctx context.Context) ([]*armcompute.VirtualMachine, error) {
			res, err := azurePager.NextPage(ctx)
			return res.Value, trace.Wrap(err)
		},
	}
}

// ListVirtualMachines lists all virtual machines in a given resource group.
// If resourceGroup is "*", it lists all virtual machines in any resource group.
func (c *vmClient) ListVirtualMachines(ctx context.Context, resourceGroup string) ([]*armcompute.VirtualMachine, error) {
	var pager vmPager
	if resourceGroup == types.Wildcard {
		pager = newListAllPager(c.api.NewListAllPager(&armcompute.VirtualMachinesClientListAllOptions{}))
	} else {
		pager = newListPager(c.api.NewListPager(resourceGroup, &armcompute.VirtualMachinesClientListOptions{}))
	}
	var virtualMachines []*armcompute.VirtualMachine
	for pager.more() {
		res, err := pager.nextPage(ctx)
		if err != nil {
			return nil, trace.Wrap(ConvertResponseError(err))
		}
		virtualMachines = append(virtualMachines, res...)
	}

	return virtualMachines, nil
}

// ListNonRunningVirtualMachineStates returns known non-running VM power states keyed by
// resource ID. Uses StatusOnly=true on a subscription-wide ListAll. Omits VMs that
// cannot be reconciled by vm.ID, as well as VMs whose power state is running or
// indeterminate.
func (c *vmClient) ListNonRunningVirtualMachineStates(ctx context.Context) (map[string]PowerState, error) {
	pager := newListAllPager(c.api.NewListAllPager(&armcompute.VirtualMachinesClientListAllOptions{
		StatusOnly: to.Ptr("true"),
	}))

	nonRunning := make(map[string]PowerState)
	for pager.more() {
		res, err := pager.nextPage(ctx)
		if err != nil {
			return nil, trace.Wrap(ConvertResponseError(err))
		}

		for _, vm := range res {
			resourceID := StringVal(vm.ID)
			if resourceID == "" {
				continue
			}
			if vm.Properties == nil || vm.Properties.InstanceView == nil {
				// No InstanceView — skip VM. Caller treats "not in map" as "running or indeterminate."
				continue
			}
			state := ParsePowerState(vm.Properties.InstanceView.Statuses)
			if state == PowerStateUnknown || state == PowerStateRunning {
				continue
			}
			nonRunning[resourceID] = state
		}
	}

	return nonRunning, nil
}

// RunCommandRequest combines parameters for running a command on an Azure virtual machine.
type RunCommandRequest struct {
	// Region is the region of the VM.
	Region string
	// ResourceGroup is the resource group for the VM.
	ResourceGroup string
	// VMName is the name of the VM.
	VMName string
	// Script is the shell script to be executed in the virtual machine.
	Script string
}

// RunCommandClient is a client for Azure Run Commands.
type RunCommandClient interface {
	Run(ctx context.Context, req RunCommandRequest) error
}

type runCommandClient struct {
	api *armcompute.VirtualMachineRunCommandsClient
}

// NewRunCommandClient creates a new Azure Run Command client by subscription
// and credentials.
func NewRunCommandClient(subscription string, cred azcore.TokenCredential, options *arm.ClientOptions) (RunCommandClient, error) {
	runCommandAPI, err := armcompute.NewVirtualMachineRunCommandsClient(subscription, cred, options)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &runCommandClient{
		api: runCommandAPI,
	}, nil
}

// Run runs a command on a virtual machine.
func (c *runCommandClient) Run(ctx context.Context, req RunCommandRequest) error {
	// TODO(Tener): make the run command name actual parameter.
	const runCommandName = "teleport-install"

	poller, err := c.api.BeginCreateOrUpdate(ctx, req.ResourceGroup, req.VMName, runCommandName, armcompute.VirtualMachineRunCommand{
		Location: to.Ptr(req.Region),
		Properties: &armcompute.VirtualMachineRunCommandProperties{
			AsyncExecution: to.Ptr(false),
			Source: &armcompute.VirtualMachineRunCommandScriptSource{
				Script: to.Ptr(req.Script),
			},
		},
	}, nil)
	if err != nil {
		return trace.Wrap(err)
	}

	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return trace.Wrap(err)
	}

	// note: we are not guaranteed to receive the output of the command above if the req.Name is not unique.
	resp, err := c.api.GetByVirtualMachine(ctx, req.ResourceGroup, req.VMName, runCommandName, &armcompute.VirtualMachineRunCommandsClientGetByVirtualMachineOptions{
		Expand: to.Ptr("instanceView"),
	})
	if err != nil {
		return trace.Wrap(err)
	}

	if resp.Properties == nil || resp.Properties.InstanceView == nil {
		return trace.BadParameter("unable to query command execution state, failure assumed")
	}
	iv := resp.Properties.InstanceView
	execState := fromPtr(iv.ExecutionState)
	if execState != armcompute.ExecutionStateSucceeded {
		return trace.BadParameter("execution failed; exec state: %v, output: %v, stderr: %v, exit code: %v", execState, fromPtr(iv.Output), fromPtr(iv.Error), fromPtr(iv.ExitCode))
	}
	return nil
}

func fromPtr[T any](ptr *T) T {
	var out T
	if ptr != nil {
		out = *ptr
	}
	return out
}
