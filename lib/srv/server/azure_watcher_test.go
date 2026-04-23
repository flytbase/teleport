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
	vms                []*armcompute.VirtualMachine
	vmsByResourceGroup map[string][]*armcompute.VirtualMachine
	nonRunning         map[string]azure.PowerState
	nonRunningCalls    int
	nonRunningErr      error
	beforeNonRunning   func(context.Context)
}

func (*countingVirtualMachinesClient) Get(context.Context, string) (*azure.VirtualMachine, error) {
	return nil, nil
}

func (*countingVirtualMachinesClient) GetByVMID(context.Context, string) (*azure.VirtualMachine, error) {
	return nil, nil
}

func (c *countingVirtualMachinesClient) ListVirtualMachines(_ context.Context, resourceGroup string) ([]*armcompute.VirtualMachine, error) {
	if c.vmsByResourceGroup == nil {
		return c.vms, nil
	}
	if resourceGroup == types.Wildcard {
		var all []*armcompute.VirtualMachine
		for _, vms := range c.vmsByResourceGroup {
			all = append(all, vms...)
		}
		return all, nil
	}
	return c.vmsByResourceGroup[resourceGroup], nil
}

func (c *countingVirtualMachinesClient) ListNonRunningVirtualMachineStates(ctx context.Context) (map[string]azure.PowerState, error) {
	return c.listNonRunningVirtualMachineStates(ctx)
}

func (c *countingVirtualMachinesClient) listNonRunningVirtualMachineStates(ctx context.Context) (map[string]azure.PowerState, error) {
	c.nonRunningCalls++
	if c.beforeNonRunning != nil {
		c.beforeNonRunning(ctx)
	}
	if c.nonRunningErr != nil {
		return nil, c.nonRunningErr
	}
	if c.nonRunning == nil {
		return map[string]azure.PowerState{}, nil
	}
	return c.nonRunning, nil
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
							Name:     to.Ptr("vm1"),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg1", "vm2")),
							Name:     to.Ptr("vm2"),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg1", "vm5")),
							Name:     to.Ptr("vm5"),
							Location: to.Ptr("location2"),
						},
					},
					"rg2": {
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm3")),
							Name:     to.Ptr("vm3"),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm4")),
							Name:     to.Ptr("vm4"),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub1, "rg2", "vm6")),
							Name:     to.Ptr("vm6"),
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
							Name:     to.Ptr("vm7"),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg3", "vm8")),
							Name:     to.Ptr("vm8"),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg3", "vm9")),
							Name:     to.Ptr("vm9"),
							Location: to.Ptr("location2"),
						},
					},
					"rg4": {
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm10")),
							Name:     to.Ptr("vm10"),
							Location: to.Ptr("location1"),
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm11")),
							Name:     to.Ptr("vm11"),
							Location: to.Ptr("location1"),
							Tags: map[string]*string{
								"teleport": to.Ptr("yes"),
							},
						},
						{
							ID:       to.Ptr(makeAzureVMID(sub2, "rg4", "vm12")),
							Name:     to.Ptr("vm12"),
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

	// runFilter constructs a single azureInstanceFetcher for the given matcher and returns the names
	// of VMs in the emitted AzureInstances. Fetcher behavior is tested directly here — watcher-level
	// integration is covered by TestAzureWatcher at the top of this file.
	runFilter := func(t *testing.T, matcher types.AzureMatcher, clients azure.Clients) []string {
		t.Helper()
		resourceGroup := types.Wildcard
		if len(matcher.ResourceGroups) > 0 {
			resourceGroup = matcher.ResourceGroups[0]
		}
		fetcher := newAzureInstanceFetcher(azureFetcherConfig{
			Matcher:       matcher,
			Subscription:  sub,
			ResourceGroup: resourceGroup,
			AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
				return clients, nil
			},
			Logger: logtest.NewLogger(),
		})
		results, err := fetcher.GetInstances(t.Context(), false)
		require.NoError(t, err)
		var vmNames []string
		for _, group := range results {
			for _, vm := range group.Instances {
				vmNames = append(vmNames, *vm.Name)
			}
		}
		return vmNames
	}

	t.Run("single wildcard matcher filters non-running VMs", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		vmNames := runFilter(t, matcher, &clients)

		require.ElementsMatch(t, []string{"vm-running"}, vmNames,
			"only running VMs should pass through power-state filter")
	})

	t.Run("all running VMs pass through filter", func(t *testing.T) {
		// Happy-path coverage: without at least one all-running fixture, a future regression that
		// accidentally drops running VMs would still pass the existing subtests that only assert
		// running VMs survive among mixed states.
		allRunning := mockClients{
			vmClients: map[string]azure.VirtualMachinesClient{
				sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
					VirtualMachines: map[string][]*armcompute.VirtualMachine{
						"rg1": {
							buildVM("rg1", "vm-a", "running"),
							buildVM("rg1", "vm-b", "running"),
							buildVM("rg1", "vm-c", "running"),
						},
					},
				}, nil),
			},
		}

		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		vmNames := runFilter(t, matcher, &allRunning)

		require.ElementsMatch(t, []string{"vm-a", "vm-b", "vm-c"}, vmNames,
			"all running VMs must pass through the filter without loss")
	})

	t.Run("wildcard matcher skips power-state filtering when bulk status fetch fails", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		failClients := mockClients{
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

		vmNames := runFilter(t, matcher, &failClients)

		require.ElementsMatch(t, []string{"vm-running", "vm-starting", "vm-deallocated", "vm-stopped"}, vmNames,
			"wildcard fetchers should fail open when the bulk status fetch fails")
	})

	t.Run("wildcard matcher allows VM through when bulk state omits it", func(t *testing.T) {
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

		missingStateClients := mockClients{
			vmClients: map[string]azure.VirtualMachinesClient{
				sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
					VirtualMachines: map[string][]*armcompute.VirtualMachine{
						"rg1": {
							buildVM("rg1", "vm-running", "running"),
							vmMissingFromBulk,
						},
					},
				}, nil),
			},
		}

		vmNames := runFilter(t, matcher, &missingStateClients)

		require.ElementsMatch(t, []string{"vm-running", "vm-missing"}, vmNames,
			"VMs missing from the bulk non-running map should fail open")
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
	require.Zero(t, client.nonRunningCalls,
		"wildcard fetchers should skip the bulk non-running scan when no VMs match local filters")
}

func TestAzureWatcher_CanceledDuringPowerStateFetchDoesNotReturnInstances(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	ctx, cancel := context.WithCancel(t.Context())
	client := &countingVirtualMachinesClient{
		vms: []*armcompute.VirtualMachine{
			{
				ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-running")),
				Name:     to.Ptr("vm-running"),
				Location: to.Ptr("eastus"),
				Properties: &armcompute.VirtualMachineProperties{
					VMID: to.Ptr("vmid-running"),
				},
			},
		},
		nonRunningErr: context.Canceled,
		beforeNonRunning: func(context.Context) {
			cancel()
		},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		},
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &mockClients{vmClients: map[string]azure.VirtualMachinesClient{sub: client}}, nil
		},
		Logger: logtest.NewLogger(),
	})

	results, err := fetcher.GetInstances(ctx, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, results,
		"GetInstances must not hand an unfiltered candidate set to the installer during shutdown")
	require.Equal(t, 1, client.nonRunningCalls,
		"cancellation should happen after the bulk non-running fetch path is entered")
}

func TestAzureWatcher_NonWildcardReconcilesAgainstSubscriptionNonRunningEntries(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	client := &countingVirtualMachinesClient{
		vmsByResourceGroup: map[string][]*armcompute.VirtualMachine{
			"rg1": {
				{
					ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-rg1-running")),
					Name:     to.Ptr("vm-rg1-running"),
					Location: to.Ptr("eastus"),
					Properties: &armcompute.VirtualMachineProperties{
						VMID: to.Ptr("vmid-rg1-running"),
					},
				},
				{
					ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-rg1-stopped")),
					Name:     to.Ptr("vm-rg1-stopped"),
					Location: to.Ptr("eastus"),
					Properties: &armcompute.VirtualMachineProperties{
						VMID: to.Ptr("vmid-rg1-stopped"),
					},
				},
			},
			"rg2": {
				{
					ID:       to.Ptr(makeAzureVMID(sub, "rg2", "vm-rg2-running")),
					Name:     to.Ptr("vm-rg2-running"),
					Location: to.Ptr("eastus"),
					Properties: &armcompute.VirtualMachineProperties{
						VMID: to.Ptr("vmid-rg2-running"),
					},
				},
			},
		},
		// Simulate the bulk ListNonRunningVirtualMachineStates response as a superset:
		// the subscription-wide call returns non-running entries for VMs outside this
		// fetcher's resource group. Those entries must be ignored during local reconciliation.
		nonRunning: map[string]azure.PowerState{
			makeAzureVMID(sub, "rg1", "vm-rg1-stopped"): azure.PowerStateStopped,
			makeAzureVMID(sub, "rg2", "vm-rg2-stopped"): azure.PowerStateStopped,
		},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{"rg1"},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		},
		Subscription:  sub,
		ResourceGroup: "rg1",
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &mockClients{vmClients: map[string]azure.VirtualMachinesClient{sub: client}}, nil
		},
		Logger: logtest.NewLogger(),
	})

	results, err := fetcher.GetInstances(t.Context(), false)
	require.NoError(t, err)

	var names []string
	for _, group := range results {
		for _, vm := range group.Instances {
			names = append(names, azure.StringVal(vm.Name))
		}
	}

	require.Equal(t, []string{"vm-rg1-running"}, names,
		"non-wildcard fetcher must reconcile bulk non-running entries against candidates: running rg1 VM passes, stopped rg1 VM filtered, rg2 VM never appears")
	require.Equal(t, 1, client.nonRunningCalls,
		"non-wildcard fetcher must issue the bulk non-running call exactly once")
}

// TestAzureWatcher_GroupCandidates_NilPropertiesDoesNotPanic exercises the
// error branch in groupCandidates that runs when arm.ParseResourceID fails on
// a VM's ID. That branch logs a warning that includes the VM's VMID. If the
// log line were ever to dereference vm.Properties.VMID directly (rather than
// going through the nil-safe azure.VMID helper), a VM with both a malformed
// ID and nil Properties would panic the fetcher goroutine and halt discovery
// for the entire subscription for that poll cycle. Azure responses commonly
// have nil Properties, so this combination is not hypothetical.
func TestAzureWatcher_GroupCandidates_NilPropertiesDoesNotPanic(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	// Malformed: unparseable resource ID + nil Properties. Both fields are
	// required to reproduce the panic scenario: the unparseable ID forces the
	// error branch, the nil Properties is what a naive log line would deref.
	malformed := &armcompute.VirtualMachine{
		ID:       to.Ptr("not-a-valid-resource-id"),
		Location: to.Ptr("eastus"),
	}
	healthy := &armcompute.VirtualMachine{
		ID:       to.Ptr(makeAzureVMID(sub, "rg1", "vm-ok")),
		Name:     to.Ptr("vm-ok"),
		Location: to.Ptr("eastus"),
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:        []string{"vm"},
			Regions:      []string{types.Wildcard},
			ResourceTags: types.Labels{"*": []string{"*"}},
		},
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		Logger:        logtest.NewLogger(),
	})

	require.NotPanics(t, func() {
		byRG, candidateCount := fetcher.groupCandidates(t.Context(), []*armcompute.VirtualMachine{malformed, healthy})

		// Malformed VM was skipped by the continue in the error branch;
		// healthy VM was grouped under its inferred (rg1, eastus) bucket.
		require.Equal(t, 1, candidateCount)
		require.Len(t, byRG, 1,
			"only the healthy VM should be grouped; the malformed one must be skipped, not kill the fetcher")
		for batchGroup, vms := range byRG {
			require.Equal(t, "rg1", batchGroup.resourceGroup)
			require.Equal(t, "eastus", batchGroup.location)
			require.Len(t, vms, 1)
			require.Equal(t, "vm-ok", *vms[0].Name)
		}
	})
}

// TestAzureWatcher_GetInstances_SkipsEmptyBuckets verifies that buckets whose
// VMs were all filtered out (e.g. all VMs stopped in a given resource group)
// do not produce a spurious AzureInstances in GetInstances' result. Emitting
// empty groups would cascade into downstream "no instances found, skipping"
// log entries per (rg, region) and, if any future caller ever treats an empty
// group as a signal (e.g. to delete a discovery record), a correctness bug.
func TestAzureWatcher_GetInstances_SkipsEmptyBuckets(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	buildVM := func(rg, name, powerState string) *armcompute.VirtualMachine {
		return &armcompute.VirtualMachine{
			ID:       to.Ptr(makeAzureVMID(sub, rg, name)),
			Name:     to.Ptr(name),
			Location: to.Ptr("eastus"),
			Properties: &armcompute.VirtualMachineProperties{
				VMID: to.Ptr("vmid-" + name),
				InstanceView: &armcompute.VirtualMachineInstanceView{
					Statuses: []*armcompute.InstanceViewStatus{
						{Code: to.Ptr("PowerState/" + powerState)},
					},
				},
			},
		}
	}

	// rg-stopped holds only stopped VMs (fully filtered to empty by
	// filterNonRunning). rg-live holds one running VM. GetInstances must
	// emit exactly one AzureInstances group (rg-live) — not two.
	clients := mockClients{
		vmClients: map[string]azure.VirtualMachinesClient{
			sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
				VirtualMachines: map[string][]*armcompute.VirtualMachine{
					"rg-stopped": {
						buildVM("rg-stopped", "vm-dead-1", "stopped"),
						buildVM("rg-stopped", "vm-dead-2", "stopped"),
					},
					"rg-live": {
						buildVM("rg-live", "vm-alive", "running"),
					},
				},
			}, nil),
		},
	}

	matcher := types.AzureMatcher{
		Types:          []string{"vm"},
		Subscriptions:  []string{sub},
		ResourceGroups: []string{types.Wildcard},
		Regions:        []string{types.Wildcard},
		ResourceTags:   types.Labels{"*": []string{"*"}},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher:       matcher,
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &clients, nil
		},
		Logger: logtest.NewLogger(),
	})
	collected, err := fetcher.GetInstances(t.Context(), false)
	require.NoError(t, err)

	require.Len(t, collected, 1,
		"GetInstances must not emit AzureInstances for buckets fully filtered out — empty groups cause spurious downstream work")
	require.Equal(t, "rg-live", collected[0].ResourceGroup,
		"the single emitted group should be the one that had a surviving VM")
	require.Len(t, collected[0].Instances, 1,
		"the emitted group should hold exactly the one running VM")
	require.Equal(t, "vm-alive", *collected[0].Instances[0].Name)
}

// TestAzureWatcher_FilterNonLinux verifies that filterNonLinux runs inside
// GetInstances and drops VMs whose reported OS type is a known non-Linux type
// (e.g. Windows), while Linux VMs and VMs with no OS metadata pass through.
// This covers the behavior previously handled by the FilterLinuxVMs block in
// installAzureServers (lib/srv/discovery/discovery.go) before it was moved
// into the fetcher.
func TestAzureWatcher_FilterNonLinux(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	buildVM := func(name, osType string) *armcompute.VirtualMachine {
		vm := &armcompute.VirtualMachine{
			ID:       to.Ptr(makeAzureVMID(sub, "rg1", name)),
			Name:     to.Ptr(name),
			Location: to.Ptr("eastus"),
			Properties: &armcompute.VirtualMachineProperties{
				VMID: to.Ptr("vmid-" + name),
				InstanceView: &armcompute.VirtualMachineInstanceView{
					Statuses: []*armcompute.InstanceViewStatus{
						{Code: to.Ptr("PowerState/running")},
					},
				},
			},
		}
		if osType != "" {
			vm.Properties.StorageProfile = &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{
					OSType: (*armcompute.OperatingSystemTypes)(to.Ptr(osType)),
				},
			}
		}
		return vm
	}

	clients := mockClients{
		vmClients: map[string]azure.VirtualMachinesClient{
			sub: azure.NewVirtualMachinesClientByAPI(&azure.ARMComputeMock{
				VirtualMachines: map[string][]*armcompute.VirtualMachine{
					"rg1": {
						buildVM("vm-linux", "Linux"),
						buildVM("vm-windows", "Windows"),
						// No OSType: passes through to avoid silently
						// dropping legitimate Linux VMs with missing metadata.
						buildVM("vm-unknown-os", ""),
					},
				},
			}, nil),
		},
	}

	matcher := types.AzureMatcher{
		Types:          []string{"vm"},
		Subscriptions:  []string{sub},
		ResourceGroups: []string{types.Wildcard},
		Regions:        []string{types.Wildcard},
		ResourceTags:   types.Labels{"*": []string{"*"}},
	}

	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher:       matcher,
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &clients, nil
		},
		Logger: logtest.NewLogger(),
	})
	collected, err := fetcher.GetInstances(t.Context(), false)
	require.NoError(t, err)

	var vmNames []string
	for _, group := range collected {
		for _, vm := range group.Instances {
			vmNames = append(vmNames, *vm.Name)
		}
	}

	require.ElementsMatch(t, []string{"vm-linux", "vm-unknown-os"}, vmNames,
		"Linux and unknown-OS VMs pass through; Windows VMs are dropped by filterNonLinux")
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
