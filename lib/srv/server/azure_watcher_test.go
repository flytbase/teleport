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
	statuses           map[string]azure.PowerState
	statusesCalls      int
	getPowerState      azure.PowerState
	getPowerStateErr   error
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

func (c *countingVirtualMachinesClient) ListVirtualMachineStates(context.Context) (map[string]azure.PowerState, error) {
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

	t.Run("duplicate wildcard matchers still filter non-running VMs", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{types.Wildcard},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		// Each of the two identical fetchers should independently filter
		// down to the one running VM. Concatenate results from both.
		allVMNames := append(
			runFilter(t, matcher, &clients),
			runFilter(t, matcher, &clients)...,
		)

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

		fallbackClients := mockClients{
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

		vmNames := runFilter(t, matcher, &fallbackClients)

		require.ElementsMatch(t, []string{"vm-running", "vm-missing"}, vmNames,
			"VMs missing from the bulk map should fail open if fallback lookup fails")
	})

	t.Run("non-wildcard matcher filters non-running VMs", func(t *testing.T) {
		matcher := types.AzureMatcher{
			Types:          []string{"vm"},
			Subscriptions:  []string{sub},
			ResourceGroups: []string{"rg1"},
			Regions:        []string{types.Wildcard},
			ResourceTags:   types.Labels{"*": []string{"*"}},
		}

		vmNames := runFilter(t, matcher, &clients)

		require.ElementsMatch(t, []string{"vm-running"}, vmNames,
			"non-wildcard resource-group fetchers should apply power-state filtering by reconciling against the RBAC-filtered bulk status response")
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

func TestAzureWatcher_NonWildcardReconcilesAgainstSubscriptionStatuses(t *testing.T) {
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
		// Simulate the bulk ListVirtualMachineStates response as a
		// superset: the subscription-wide call returns statuses for VMs
		// outside this fetcher's resource group. Those entries must be
		// ignored during local reconciliation.
		statuses: map[string]azure.PowerState{
			makeAzureVMID(sub, "rg1", "vm-rg1-running"): azure.PowerStateRunning,
			makeAzureVMID(sub, "rg1", "vm-rg1-stopped"): azure.PowerStateStopped,
			makeAzureVMID(sub, "rg2", "vm-rg2-running"): azure.PowerStateRunning,
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
		"non-wildcard fetcher must reconcile bulk status against candidates: running rg1 VM passes, stopped rg1 VM filtered, rg2 VM never appears")
	require.Equal(t, 1, client.statusesCalls,
		"non-wildcard fetcher must issue the bulk status call exactly once")
}

func TestAzureWatcher_FallbackLookupCap(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	// Build VMs without InstanceView — these will be present in
	// ListVirtualMachines but missing from ListVirtualMachineStates
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
		byRG := fetcher.groupCandidates(t.Context(), []*armcompute.VirtualMachine{malformed, healthy})

		// Malformed VM was skipped by the continue in the error branch;
		// healthy VM was grouped under its inferred (rg1, eastus) bucket.
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

// TestAzureWatcher_FilterNonRunning_DeterministicAcrossRGs verifies that when the
// per-fetch fallback budget is exhausted across multiple resource groups, the same
// resource groups consume the budget on every invocation — i.e. the same stopped
// VM never "wins" the fallback on one poll and "loses" it on the next. Go's
// map iteration order is randomized, so filterNonRunning must iterate batch
// groups in a stable (resourceGroup, location) order.
//
// Fixture: two RGs with 7 VMs each (14 total), all missing from the bulk map,
// cap = 10. With sorted iteration, "rg-a" consumes the full 7-slot first burst,
// "rg-b" uses the next 3, and the remaining 4 in "rg-b" fail open. Without
// sorting, which RG's VMs fail open flips between invocations.
func TestAzureWatcher_FilterNonRunning_DeterministicAcrossRGs(t *testing.T) {
	t.Parallel()

	const sub = "00000000-0000-0000-0000-000000000000"

	buildVMs := func(rg string) []*armcompute.VirtualMachine {
		var vms []*armcompute.VirtualMachine
		for i := range 7 {
			name := fmt.Sprintf("%s-vm-%d", rg, i)
			vms = append(vms, &armcompute.VirtualMachine{
				ID:       to.Ptr(makeAzureVMID(sub, rg, name)),
				Name:     to.Ptr(name),
				Location: to.Ptr("eastus"),
			})
		}
		return vms
	}

	client := &countingVirtualMachinesClient{
		getPowerState: azure.PowerStateStopped,
	}
	fetcher := newAzureInstanceFetcher(azureFetcherConfig{
		Matcher: types.AzureMatcher{
			Types:        []string{"vm"},
			Regions:      []string{types.Wildcard},
			ResourceTags: types.Labels{"*": []string{"*"}},
		},
		Subscription:  sub,
		ResourceGroup: types.Wildcard,
		AzureClientGetter: func(context.Context, string) (azure.Clients, error) {
			return &mockClients{vmClients: map[string]azure.VirtualMachinesClient{sub: client}}, nil
		},
		Logger: logtest.NewLogger(),
	})

	rgALoc := resourceGroupLocation{resourceGroup: "rg-a", location: "eastus"}
	rgBLoc := resourceGroupLocation{resourceGroup: "rg-b", location: "eastus"}

	// Enough iterations to shake out Go's map-iteration randomness if the
	// fix is removed. With the sort in place, every iteration produces the
	// same sorted-order result.
	const iterations = 50
	for i := range iterations {
		byRG := map[resourceGroupLocation][]*armcompute.VirtualMachine{
			rgALoc: buildVMs("rg-a"),
			rgBLoc: buildVMs("rg-b"),
		}

		filtered, stats := fetcher.filterNonRunning(t.Context(), client, byRG, map[string]azure.PowerState{})

		require.Equal(t, maxPowerStateFallbackLookupsPerFetch, stats.fallbackLookups,
			"iteration %d: should hit cap after maxPowerStateFallbackLookupsPerFetch lookups", i)
		require.Equal(t, 4, stats.fallbackLookupsSkipped,
			"iteration %d: 4 VMs beyond the cap should skip fallback and fail open", i)
		require.Equal(t, 10, stats.filteredNonRunning,
			"iteration %d: all 10 fallback lookups return stopped and are filtered", i)

		// Input map must not be mutated.
		require.Len(t, byRG[rgALoc], 7,
			"iteration %d: filterNonRunning must not mutate its input — rg-a bucket still has its 7 VMs", i)
		require.Len(t, byRG[rgBLoc], 7,
			"iteration %d: filterNonRunning must not mutate its input — rg-b bucket still has its 7 VMs", i)

		// rg-a sorts first → consumes 7 of 10 budget slots → all filtered.
		require.Empty(t, filtered[rgALoc],
			"iteration %d: rg-a consumed the fallback budget first; all its stopped VMs should be filtered", i)

		// rg-b uses next 3 slots → filtered; remaining 4 fail open.
		var rgBNames []string
		for _, vm := range filtered[rgBLoc] {
			rgBNames = append(rgBNames, *vm.Name)
		}
		require.Equal(t,
			[]string{"rg-b-vm-3", "rg-b-vm-4", "rg-b-vm-5", "rg-b-vm-6"}, rgBNames,
			"iteration %d: rg-b's last 4 VMs (after the 3 that got fallback slots) should fail open in slice order", i)
	}
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
