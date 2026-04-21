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

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/cloud/azure"
	"github.com/gravitational/teleport/lib/utils/log/logtest"
)

type mockClients struct {
	azure.Clients

	vmClients map[string]azure.VirtualMachinesClient
}

func (c *mockClients) GetVirtualMachinesClient(ctx context.Context, subscription string) (azure.VirtualMachinesClient, error) {
	vmClient, ok := c.vmClients[subscription]
	if !ok {
		return nil, trace.NotFound("subscription %s not found", subscription)
	}
	return vmClient, nil
}

type countingVirtualMachinesClient struct {
	vms              []*armcompute.VirtualMachine
	statuses         map[string]azure.PowerState
	statusesCalls    int
	getPowerState    azure.PowerState
	getPowerStateErr error
}

func (*countingVirtualMachinesClient) Get(context.Context, string) (*azure.VirtualMachine, error) {
	return nil, nil
}

func (*countingVirtualMachinesClient) GetByVMID(context.Context, string) (*azure.VirtualMachine, error) {
	return nil, nil
}

func (c *countingVirtualMachinesClient) ListVirtualMachines(context.Context, string) ([]*armcompute.VirtualMachine, error) {
	return c.vms, nil
}

func (c *countingVirtualMachinesClient) ListVirtualMachineStatuses(context.Context) (map[string]azure.PowerState, error) {
	c.statusesCalls++
	return c.statuses, nil
}

func (c *countingVirtualMachinesClient) GetVMPowerState(context.Context, string, string) (azure.PowerState, error) {
	return c.getPowerState, c.getPowerStateErr
}

func TestAzureWatcher(t *testing.T) {
	t.Parallel()

	const (
		sub1 = "00000000-0000-0000-0000-000000000000"
		sub2 = "11111111-1111-1111-1111-111111111111"
	)
	clients := mockClients{
		vmClients: map[string]azure.VirtualMachinesClient{
			sub1: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
				VirtualMachines: map[string][]*armcompute.VirtualMachine{
					"rg1": {
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg1", "vm1")),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg1", "vm2")),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg1", "vm5")),
							Location: to.Ptr("location2"),
						},
					},
					"rg2": {
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm3")),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm4")),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm6")),
							Location: to.Ptr("location2"),
						},
					},
				},
			}, nil /* scaleSetAPI */),
			sub2: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
				VirtualMachines: map[string][]*armcompute.VirtualMachine{
					"rg3": {
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg3", "vm7")),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg3", "vm8")),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg3", "vm9")),
							Location: to.Ptr("location2"),
						},
					},
					"rg4": {
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm10")),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm11")),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm12")),
							Location: to.Ptr("location2"),
						},
					},
				},
			}, nil /* scaleSetAPI */),
		},
	}

	tests := []struct {
		name    string
		matcher types.AzureMatcher
		wantVMs []string
	}{
		{
			name: "all vms in a subscription",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1", "rg2"},
				Regions:        []string{"location1", "location2"},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm1", "vm2", "vm3", "vm4", "vm5", "vm6"},
		},
		{
			name: "filter by resource group",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1"},
				Regions:        []string{"location1", "location2"},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm1", "vm2", "vm5"},
		},
		{
			name: "filter by location",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1", "rg2"},
				Regions:        []string{"location2"},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm5", "vm6"},
		},
		{
			name: "filter by tag",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1", "rg2"},
				Regions:        []string{"location1", "location2"},
				ResourceTags:   types.Labels{"teleport": []string{"yes"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm2", "vm4"},
		},
		{
			name: "location wildcard",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1", "rg2"},
				Regions:        []string{types.Wildcard},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm1", "vm2", "vm3", "vm4", "vm5", "vm6"},
		},
		{
			name: "resource group wildcard",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"*"},
				Regions:        []string{types.Wildcard},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{sub1},
			},
			wantVMs: []string{"vm1", "vm2", "vm3", "vm4", "vm5", "vm6"},
		},
		{
			name: "subscription wildcard",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"rg1", "rg4"},
				Regions:        []string{types.Wildcard},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{"*"},
			},
			wantVMs: []string{"vm1", "vm2", "vm5", "vm10", "vm11", "vm12"},
		},
		{
			name: "subscription wildcard with resource group wildcard",
			matcher: types.AzureMatcher{
				ResourceGroups: []string{"*"},
				Regions:        []string{types.Wildcard},
				ResourceTags:   types.Labels{"*": []string{"*"}},
				Subscriptions:  []string{"*"},
			},
			wantVMs: []string{"vm1", "vm2", "vm3", "vm4", "vm5", "vm6", "vm7", "vm8", "vm9", "vm10", "vm11", "vm12"},
		},
	}

	logger := logtest.NewLogger()
	for _, tc := range tests {
		tc.matcher.Types = []string{"vm"}

		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			watcher := NewWatcher[*AzureInstances](ctx)

			const noDiscoveryConfig = ""
			watcher.SetFetchers(noDiscoveryConfig,
				MatchersToAzureInstanceFetchers(
					t.Context(),
					logger,
					[]types.AzureMatcher{tc.matcher},
					func(ctx context.Context, integration string) (azure.Clients, error) {
						return &clients, nil
					},
					noDiscoveryConfig,
					func(ctx context.Context, integration string) (subscriptions []string, err error) {
						return []string{sub1, sub2}, nil
					},
				),
			)

			go watcher.Run()
			t.Cleanup(watcher.Stop)

			var vmIDs []string

			for len(vmIDs) < len(tc.wantVMs) {
				select {
				case results := <-watcher.InstancesC:
					for _, vm := range results.Instances {
						parsedResource, err := arm.ParseResourceID(*vm.ID)
						require.NoError(t, err)
						vmID := parsedResource.Name
						vmIDs = append(vmIDs, vmID)
					}
					require.NotEqual(t, "*", results.ResourceGroup, "Discovered VM's ResourceGroup should never be the wildcard")
					require.NotEqual(t, "*", results.SubscriptionID, "Discovered VM's SubscriptionID should never be the wildcard")
				case <-ctx.Done():
					require.ElementsMatch(t, tc.wantVMs, vmIDs, "timed out while waiting for expected VMs")
				}
			}

			require.ElementsMatch(t, tc.wantVMs, vmIDs)
		})
	}
}

func TestAzureInstances_FilterExistingNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		instances     *AzureInstances
		existingNodes []types.Server
		expectedVMIDs []string
	}{
		{
			name: "no existing nodes",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-1"),
						},
					},
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm2"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-2"),
						},
					},
				},
			},
			existingNodes: []types.Server{},
			expectedVMIDs: []string{"vm-id-1", "vm-id-2"},
		},
		{
			name: "filter out matching node",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-1"),
						},
					},
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm2"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-2"),
						},
					},
				},
			},
			existingNodes: []types.Server{
				makeAzureNode(t, "node-1", "sub-1", "vm-id-1"),
			},
			expectedVMIDs: []string{"vm-id-2"},
		},
		{
			name: "filter out all matching nodes",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-1"),
						},
					},
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm2"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-2"),
						},
					},
				},
			},
			existingNodes: []types.Server{
				makeAzureNode(t, "node-1", "sub-1", "vm-id-1"),
				makeAzureNode(t, "node-2", "sub-1", "vm-id-2"),
			},
			expectedVMIDs: []string{},
		},
		{
			name: "different subscription is not filtered",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-1"),
						},
					},
				},
			},
			existingNodes: []types.Server{
				makeAzureNode(t, "node-1", "sub-2", "vm-id-1"),
			},
			expectedVMIDs: []string{"vm-id-1"},
		},
		{
			name: "node without vm id is not used for filtering",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID: to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: &armcompute.VirtualMachineProperties{
							VMID: to.Ptr("vm-id-1"),
						},
					},
				},
			},
			existingNodes: []types.Server{
				makeAzureNode(t, "node-1", "sub-1", ""),
			},
			expectedVMIDs: []string{"vm-id-1"},
		},
		{
			name: "instance without properties is not filtered",
			instances: &AzureInstances{
				SubscriptionID: "sub-1",
				Instances: []*armcompute.VirtualMachine{
					{
						ID:         to.Ptr("/subscriptions/sub-1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1"),
						Properties: nil,
					},
				},
			},
			existingNodes: []types.Server{
				makeAzureNode(t, "node-1", "sub-1", "vm-id-1"),
			},
			expectedVMIDs: []string{""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.instances.FilterExistingNodes(tc.existingNodes)

			var gotVMIDs []string
			for _, vm := range tc.instances.Instances {
				var vmID string
				if vm.Properties != nil {
					vmID = *vm.Properties.VMID
				}
				gotVMIDs = append(gotVMIDs, vmID)
			}

			require.ElementsMatch(t, tc.expectedVMIDs, gotVMIDs)
		})
	}
}

func TestAzureWatcher_PowerStateFiltering(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	buildVM := func(rg, name, powerState string) *armcompute.VirtualMachine {
		statuses := []*armcompute.InstanceViewStatus{
			{Code: to.Ptr("ProvisioningState/succeeded")},
		}
		if powerState != "" {
			statuses = append(statuses,
				&armcompute.InstanceViewStatus{
					Code: to.Ptr("PowerState/" + powerState),
				})
		}
		return &armcompute.VirtualMachine{
			ID:       to.Ptr(makeAzureVMID(sub, rg, name)),
			Name:     to.Ptr(name),
			Location: to.Ptr("eastus"),
			Properties: &armcompute.VirtualMachineProperties{
				VMID: to.Ptr("vmid-" + name),
				InstanceView: &armcompute.VirtualMachineInstanceView{
					Statuses: statuses,
				},
			},
		}
	}

	clients := mockClients{
		vmClients: map[string]azure.VirtualMachinesClient{
			sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
				VirtualMachines: map[string][]*armcompute.VirtualMachine{
					"rg1": {
						buildVM("rg1", "vm-running", "running"),
						buildVM("rg1", "vm-starting", "starting"),
						buildVM("rg1", "vm-deallocated", "deallocated"),
						buildVM("rg1", "vm-stopped", "stopped"),
					},
				},
			}, nil),
		},
	}

	logger := logtest.NewLogger()

	t.Run("single wildcard matcher filters non-running VMs", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		watcher := NewWatcher[*AzureInstances](ctx)

		const noDiscoveryConfig = ""
		watcher.SetFetchers(noDiscoveryConfig,
			MatchersToAzureInstanceFetchers(
				t.Context(), logger,
				[]types.AzureMatcher{matcher},
				func(context.Context, string) (azure.Clients, error) {
					return &clients, nil
				},
				noDiscoveryConfig,
				func(context.Context, string) ([]string, error) {
					return []string{sub}, nil
				},
			),
		)

		go watcher.Run()
		t.Cleanup(watcher.Stop)

		var vmNames []string
		select {
		case results := <-watcher.InstancesC:
			for _, vm := range results.Instances {
				vmNames = append(vmNames, *vm.Name)
			}
		case <-ctx.Done():
			require.Fail(t, "timed out waiting for watcher results")
		}

		require.ElementsMatch(t, []string{"vm-running"}, vmNames,
			"only running VMs should pass through power-state filter")
	})

	t.Run("duplicate wildcard matchers still filter non-running VMs", func(t *testing.T) {
		matcher1 := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}
		matcher2 := matcher1

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		watcher := NewWatcher[*AzureInstances](ctx)

		const noDiscoveryConfig = ""
		watcher.SetFetchers(noDiscoveryConfig,
			MatchersToAzureInstanceFetchers(
				t.Context(), logger,
				[]types.AzureMatcher{matcher1, matcher2},
				func(context.Context, string) (azure.Clients, error) {
					return &clients, nil
				},
				noDiscoveryConfig,
				func(context.Context, string) ([]string, error) {
					return []string{sub}, nil
				},
			),
		)

		go watcher.Run()
		t.Cleanup(watcher.Stop)

		// Both fetchers should produce results, but only running VMs.
		var allVMNames []string
		for range 2 {
			select {
			case results := <-watcher.InstancesC:
				for _, vm := range results.Instances {
					allVMNames = append(allVMNames, *vm.Name)
				}
			case <-ctx.Done():
				require.Fail(t, "timed out waiting for watcher results")
			}
		}

		// Each fetcher returns only "vm-running", so we expect it
		// twice. Crucially, vm-deallocated and vm-stopped must not
		// appear.
		require.ElementsMatch(t, []string{"vm-running", "vm-running"}, allVMNames,
			"only running VMs should pass through for each wildcard fetcher")
	})

	t.Run("wildcard matcher skips power-state filtering when bulk status fetch fails", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		clients := mockClients{
			vmClients: map[string]azure.VirtualMachinesClient{
				sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
					VirtualMachines: map[string][]*armcompute.VirtualMachine{
						"rg1": {
							buildVM("rg1", "vm-running", "running"),
							buildVM("rg1", "vm-starting", "starting"),
							buildVM("rg1", "vm-deallocated", "deallocated"),
							buildVM("rg1", "vm-stopped", "stopped"),
						},
					},
					StatusOnlyErr: fmt.Errorf("bulk status fetch failed"),
				}, nil),
			},
		}

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		watcher := NewWatcher[*AzureInstances](ctx)

		const noDiscoveryConfig = ""
		watcher.SetFetchers(noDiscoveryConfig,
			MatchersToAzureInstanceFetchers(
				t.Context(), logger,
				[]types.AzureMatcher{matcher},
				func(context.Context, string) (azure.Clients, error) {
					return &clients, nil
				},
				noDiscoveryConfig,
				func(context.Context, string) ([]string, error) {
					return []string{sub}, nil
				},
			),
		)

		go watcher.Run()
		t.Cleanup(watcher.Stop)

		var vmNames []string
		select {
		case results := <-watcher.InstancesC:
			for _, vm := range results.Instances {
				vmNames = append(vmNames, *vm.Name)
			}
		case <-ctx.Done():
			require.Fail(t, "timed out waiting for watcher results")
		}

		require.ElementsMatch(t, []string{"vm-running", "vm-starting", "vm-deallocated", "vm-stopped"}, vmNames,
			"wildcard fetchers should fail open when the bulk status fetch fails")
	})

	t.Run("wildcard matcher allows VM through when fallback lookup fails", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		vmMissingFromBulk := &armcompute.VirtualMachine{
			ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-missing")),
			Name:     to.Ptr("vm-missing"),
			Location: to.Ptr("eastus"),
			Properties: &armcompute.VirtualMachineProperties{
				VMID: to.Ptr("vmid-missing"),
			},
		}

		clients := mockClients{
			vmClients: map[string]azure.VirtualMachinesClient{
				sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
					VirtualMachines: map[string][]*armcompute.VirtualMachine{
						"rg1": {
							buildVM("rg1", "vm-running", "running"),
							vmMissingFromBulk,
						},
					},
					GetErr: fmt.Errorf("fallback lookup failed"),
				}, nil),
			},
		}

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		watcher := NewWatcher[*AzureInstances](ctx)

		const noDiscoveryConfig = ""
		watcher.SetFetchers(noDiscoveryConfig,
			MatchersToAzureInstanceFetchers(
				t.Context(), logger,
				[]types.AzureMatcher{matcher},
				func(context.Context, string) (azure.Clients, error) {
					return &clients, nil
				},
				noDiscoveryConfig,
				func(context.Context, string) ([]string, error) {
					return []string{sub}, nil
				},
			),
		)

		go watcher.Run()
		t.Cleanup(watcher.Stop)

		var vmNames []string
		select {
		case results := <-watcher.InstancesC:
			for _, vm := range results.Instances {
				vmNames = append(vmNames, *vm.Name)
			}
		case <-ctx.Done():
			require.Fail(t, "timed out waiting for watcher results")
		}

		require.ElementsMatch(t, []string{"vm-running", "vm-missing"}, vmNames,
			"VMs missing from the bulk map should fail open if fallback lookup fails")
	})

	t.Run("non-wildcard matcher skips power-state filtering", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{"rg1"},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		watcher := NewWatcher[*AzureInstances](ctx)

		const noDiscoveryConfig = ""
		watcher.SetFetchers(noDiscoveryConfig,
			MatchersToAzureInstanceFetchers(
				t.Context(), logger,
				[]types.AzureMatcher{matcher},
				func(context.Context, string) (azure.Clients, error) {
					return &clients, nil
				},
				noDiscoveryConfig,
				func(context.Context, string) ([]string, error) {
					return []string{sub}, nil
				},
			),
		)

		go watcher.Run()
		t.Cleanup(watcher.Stop)

		var vmNames []string
		select {
		case results := <-watcher.InstancesC:
			for _, vm := range results.Instances {
				vmNames = append(vmNames, *vm.Name)
			}
		case <-ctx.Done():
			require.Fail(t, "timed out waiting for watcher results")
		}

		require.ElementsMatch(t, []string{"vm-running", "vm-starting", "vm-deallocated", "vm-stopped"}, vmNames,
			"specific resource-group fetchers should skip power-state filtering")
	})
}

func TestAzureWatcher_SkipBulkStatusFetchWhenNoCandidates(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	client := &countingVirtualMachinesClient{
		vms: []*armcompute.VirtualMachine{
			{
				ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-prod")),
				Name:     to.Ptr("vm-prod"),
				Location: to.Ptr("eastus"),
				Tags: map[string]*string{
					"env": to.Ptr("prod"),
				},
				Properties: &armcompute.VirtualMachineProperties{
					VMID: to.Ptr("vmid-prod"),
				},
			},
		},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"teleport": []string{"yes"}},
		},
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &mockClients{vmClients: map[string]azure.VirtualMachinesClient{sub: client}}, nil
		},
		Logger: logtest.NewLogger(),
	})

	results, err := fetcher.GetInstances(t.Context(), false)
	require.NoError(t, err)
	require.Empty(t, results)
	require.Zero(t, client.statusesCalls,
		"wildcard fetchers should skip the bulk status scan when no VMs match local filters")
}

func TestAzureWatcher_FallbackLookupCap(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	// Build VMs without InstanceView — these will be present in
	// ListVirtualMachines but missing from ListVirtualMachineStatuses
	// (which only returns VMs with parseable InstanceView power
	// states). This forces fallback per-VM lookups in GetInstances.
	//
	// The mock's Get always returns GetResult, so we set GetResult
	// to a stopped VM. Up to maxPowerStateFallbackLookupsPerFetch
	// VMs will be looked up individually and filtered as stopped.
	// VMs beyond the cap are passed through without checking
	// (fail-open).
	vmCount := maxPowerStateFallbackLookupsPerFetch + 3
	var vms []*armcompute.VirtualMachine
	for i := range vmCount {
		name := fmt.Sprintf("vm-%02d", i)
		vms = append(vms, &armcompute.VirtualMachine{
			ID:       to.Ptr(makeAzureVMID(sub, "rg1", name)),
			Name:     to.Ptr(name),
			Location: to.Ptr("eastus"),
			Properties: &armcompute.VirtualMachineProperties{
				VMID: to.Ptr("vmid-" + name),
				// No InstanceView — will be missing from bulk
				// status map.
			},
		})
	}

	// Also add one running VM WITH InstanceView so it appears in the
	// bulk map and passes through normally.
	vms = append(vms, &armcompute.VirtualMachine{
		ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-running")),
		Name:     to.Ptr("vm-running"),
		Location: to.Ptr("eastus"),
		Properties: &armcompute.VirtualMachineProperties{
			VMID: to.Ptr("vmid-running"),
			InstanceView: &armcompute.VirtualMachineInstanceView{
				Statuses: []*armcompute.InstanceViewStatus{
					{Code: to.Ptr("PowerState/running")},
				},
			},
		},
	})

	mockAPI := &azure.ARMComputeMock{
		VirtualMachines: map[string][]*armcompute.VirtualMachine{
			"rg1": vms,
		},
		// Per-VM fallback calls Get, which returns this result.
		// Stopped state means the first 10 fallback VMs get filtered.
		GetResult: armcompute.VirtualMachine{
			Properties: &armcompute.VirtualMachineProperties{
				InstanceView: &armcompute.VirtualMachineInstanceView{
					Statuses: []*armcompute.InstanceViewStatus{
						{Code: to.Ptr("PowerState/stopped")},
					},
				},
			},
		},
	}

	clients := mockClients{
		vmClients: map[string]azure.VirtualMachinesClient{
			sub: azure.NewVirtualMachinesClientByAPI(mockAPI, nil),
		},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:        []string{"vm"},
			Regions:      []string{types.Wildcard},
			ResourceTags: types.Labels{"*": []string{"*"}},
		},
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(ctx context.Context, integration string) (azure.Clients, error) {
			return &clients, nil
		},
		Logger: logtest.NewLogger(),
	})

	results, err := fetcher.GetInstances(t.Context(), false)
	require.NoError(t, err)

	var names []string
	for _, group := range results {
		for _, vm := range group.Instances {
			names = append(names, *vm.Name)
		}
	}

	// Expected:
	// - "vm-running" passes via bulk status map.
	// - maxPowerStateFallbackLookupsPerFetch VMs are looked up
	//   individually (all return stopped → filtered out).
	// - Remaining no-InstanceView VMs exceed the cap and are
	//   passed through without a power-state check (fail-open).
	require.Contains(t, names, "vm-running",
		"VM with running state in bulk map should pass through")

	beyondCap := vmCount - maxPowerStateFallbackLookupsPerFetch
	// 1 (vm-running) + beyondCap (fail-open VMs)
	require.Len(t, names, 1+beyondCap,
		"result should contain the running VM plus only the VMs that exceeded the fallback cap")
}

func makeAzureNode(t *testing.T, name, subscriptionID, vmID string) types.Server {
	t.Helper()

	labels := map[string]string{
		types.SubscriptionIDLabelInternal: subscriptionID,
	}
	if vmID != "" {
		labels[types.VMIDLabelInternal] = vmID
	}

	node, err := types.NewServerWithLabels(name, types.KindNode, types.ServerSpecV2{}, labels)
	require.NoError(t, err)
	return node
}

func makeAzureVMID(subscription, resourceGroup, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		subscription, resourceGroup, name,
	)
}
