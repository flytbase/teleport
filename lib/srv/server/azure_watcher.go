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
	"log/slog"
	"slices"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/gravitational/trace"

	usageeventsv1 "github.com/gravitational/teleport/api/gen/proto/go/usageevents/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/installers"
	"github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/lib/cloud/azure"
	"github.com/gravitational/teleport/lib/services"
)

const azureEventPrefix = "azure/"

// maxPowerStateFallbackLookupsPerFetch caps per-VM calls when VMs are missing from the bulk StatusOnly response.
// Without a cap, an incomplete bulk response would cause O(N) individual ARM calls, the same amplification the bulk
// path is designed to avoid. VMs beyond the cap are passed through without a power-state check (fail-open).
const maxPowerStateFallbackLookupsPerFetch = 10

const (
	powerFilterReasonNonWildcardResourceGroup = "non_wildcard_resource_group"
	powerFilterReasonNoCandidates             = "no_candidates"
	powerFilterReasonStatusFetchError         = "status_fetch_error"
)

// AzureInstances contains information about discovered Azure virtual machines.
type AzureInstances struct {
	// DiscoveryConfigName is the name of discovery config.
	DiscoveryConfigName string
	// Integration is the optional name of the integration to use for auth.
	Integration string

	// Region is the Azure region where the instances are located.
	Region string
	// SubscriptionID is the subscription ID for the instances.
	SubscriptionID string
	// ResourceGroup is the resource group for the instances.
	ResourceGroup string

	// InstallerParams are the installer parameters used for installation.
	InstallerParams *types.InstallerParams
	// Instances is a list of discovered Azure virtual machines.
	Instances []*armcompute.VirtualMachine
}

// MakeEvents generates MakeEvents for these instances.
func (instances *AzureInstances) MakeEvents(failures []AzureInstallFailure) map[string]*usageeventsv1.ResourceCreateEvent {
	resourceType := types.DiscoveredResourceNode
	if instances.InstallerParams != nil && instances.InstallerParams.ScriptName == installers.InstallerScriptNameAgentless {
		resourceType = types.DiscoveredResourceAgentlessNode
	}

	failed := map[string]struct{}{}
	for _, failure := range failures {
		id := azure.StringVal(failure.Instance.ID)
		failed[id] = struct{}{}
	}

	expectedSize := len(instances.Instances) - len(failures)
	events := make(map[string]*usageeventsv1.ResourceCreateEvent, expectedSize)
	for _, inst := range instances.Instances {
		id := azure.StringVal(inst.ID)
		// skip failed
		if _, found := failed[id]; found {
			continue
		}
		events[azureEventPrefix+id] = &usageeventsv1.ResourceCreateEvent{
			ResourceType:        resourceType,
			ResourceOrigin:      types.OriginCloud,
			CloudProvider:       types.CloudAzure,
			DiscoveryConfigName: instances.DiscoveryConfigName,
		}
	}
	return events
}

// FilterExistingNodes removes instances matching existing nodes in place.
func (instances *AzureInstances) FilterExistingNodes(existingNodes []types.Server) {
	vmIDs := make(map[string]struct{})
	for _, node := range existingNodes {
		labels := node.GetAllLabels()
		subscriptionID := labels[types.SubscriptionIDLabelInternal]
		if subscriptionID != instances.SubscriptionID {
			continue
		}
		vmID := labels[types.VMIDLabelInternal]
		if vmID != "" {
			vmIDs[vmID] = struct{}{}
		}
	}

	instances.Instances = slices.DeleteFunc(instances.Instances, func(instance *armcompute.VirtualMachine) bool {
		var vmID string
		if instance.Properties != nil && instance.Properties.VMID != nil {
			vmID = *instance.Properties.VMID
		}
		_, found := vmIDs[vmID]
		return found
	})
}

type azureClientGetter func(ctx context.Context, integration string) (azure.Clients, error)

type listSubscriptionsFunc func(ctx context.Context, integration string) (subscriptions []string, err error)

// MatchersToAzureInstanceFetchers converts a list of Azure VM Matchers into a list of Azure VM Fetchers.
func MatchersToAzureInstanceFetchers(
	ctx context.Context,
	logger *slog.Logger,
	matchers []types.AzureMatcher,
	getClient azureClientGetter,
	discoveryConfigName string,
	listSubs listSubscriptionsFunc,
) []Fetcher[*AzureInstances] {
	ret := make([]Fetcher[*AzureInstances], 0)
	for _, matcher := range matchers {
		matcher.Subscriptions = expandAzureMatcherSubscriptions(ctx, logger, matcher.Subscriptions, matcher.Integration, listSubs)
		for _, subscription := range matcher.Subscriptions {
			for _, resourceGroup := range matcher.ResourceGroups {
				fetcher := newAzureInstanceFetcher(azureFetcherConfig{
					Matcher:             matcher,
					Subscription:        subscription,
					ResourceGroup:       resourceGroup,
					AzureClientGetter:   getClient,
					DiscoveryConfigName: discoveryConfigName,
					Logger:              logger,
				})
				ret = append(ret, fetcher)
			}
		}
	}
	return ret
}

// expandAzureMatcherSubscriptions fetches the subscriptions for any wildcard
// subscriptions and replaces the wildcard with the subscriptions list.
func expandAzureMatcherSubscriptions(
	ctx context.Context,
	logger *slog.Logger,
	subscriptions []string,
	integration string,
	listSubs listSubscriptionsFunc,
) []string {
	var out []string
	for _, sub := range subscriptions {
		if sub != types.Wildcard {
			out = append(out, sub)
			continue
		}
		subs, err := listSubs(ctx, integration)
		if err != nil {
			// TODO(gavin): make a user task
			logger.WarnContext(ctx, "Failed to fetch Azure subscription list for wildcard in discovery configuration",
				"integration", integration,
				"error", err,
			)
			continue
		}
		out = append(out, subs...)
	}
	return utils.Deduplicate(out)
}

type azureFetcherConfig struct {
	Matcher             types.AzureMatcher
	Subscription        string
	ResourceGroup       string
	AzureClientGetter   azureClientGetter
	DiscoveryConfigName string
	Logger              *slog.Logger
}

type azureInstanceFetcher struct {
	InstallerParams     *types.InstallerParams
	AzureClientGetter   azureClientGetter
	Regions             []string
	Subscription        string
	ResourceGroup       string
	Labels              types.Labels
	DiscoveryConfigName string
	Integration         string
	Logger              *slog.Logger
}

func newAzureInstanceFetcher(cfg azureFetcherConfig) *azureInstanceFetcher {
	return &azureInstanceFetcher{
		InstallerParams:     cfg.Matcher.Params,
		AzureClientGetter:   cfg.AzureClientGetter,
		Regions:             cfg.Matcher.Regions,
		Subscription:        cfg.Subscription,
		ResourceGroup:       cfg.ResourceGroup,
		Labels:              cfg.Matcher.ResourceTags,
		DiscoveryConfigName: cfg.DiscoveryConfigName,
		Integration:         cfg.Matcher.Integration,
		Logger:              cfg.Logger,
	}
}

func (*azureInstanceFetcher) GetMatchingInstances(_ context.Context, _ []types.Server, _ bool) ([]*AzureInstances, error) {
	return nil, trace.NotImplemented("not implemented for azure fetchers")
}

func (f *azureInstanceFetcher) GetDiscoveryConfigName() string {
	return f.DiscoveryConfigName
}

// IntegrationName identifies the integration name whose credentials were used to fetch the resources.
// Might be empty when the fetcher is using ambient credentials.
func (f *azureInstanceFetcher) IntegrationName() string {
	return f.Integration
}

type resourceGroupLocation struct {
	resourceGroup string
	location      string
}

// GetInstances fetches all Azure virtual machines matching configured filters.
func (f *azureInstanceFetcher) GetInstances(ctx context.Context, _ bool) ([]*AzureInstances, error) {
	azureClients, err := f.AzureClientGetter(ctx, f.IntegrationName())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	client, err := azureClients.GetVirtualMachinesClient(ctx, f.Subscription)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	vms, err := client.ListVirtualMachines(ctx, f.ResourceGroup)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	instByRegionAndRG := make(map[resourceGroupLocation][]*armcompute.VirtualMachine)

	allowAllLocations := slices.Contains(f.Regions, types.Wildcard)
	allowAllResourceGroups := f.ResourceGroup == types.Wildcard

	for _, vm := range vms {
		location := azure.StringVal(vm.Location)
		if !slices.Contains(f.Regions, location) && !allowAllLocations {
			continue
		}

		vmTags := make(map[string]string, len(vm.Tags))
		for key, value := range vm.Tags {
			vmTags[key] = azure.StringVal(value)
		}
		if match, _, _ := services.MatchLabels(f.Labels, vmTags); !match {
			continue
		}

		resourceGroup := f.ResourceGroup
		if allowAllResourceGroups {
			resourceMetadata, err := arm.ParseResourceID(azure.StringVal(vm.ID))
			if err != nil {
				f.Logger.WarnContext(ctx, "Skipping Teleport installation on Azure VM - failed to infer resource group from vm id",
					"subscription_id", f.Subscription,
					"vm_id", azure.StringVal(vm.Properties.VMID),
					"resource_id", azure.StringVal(vm.ID),
					"error", err,
				)
				continue
			}
			resourceGroup = resourceMetadata.ResourceGroupName
		}

		batchGroup := resourceGroupLocation{
			resourceGroup: resourceGroup,
			location:      location,
		}

		instByRegionAndRG[batchGroup] = append(instByRegionAndRG[batchGroup], vm)
	}

	candidateCount := 0
	for _, grouped := range instByRegionAndRG {
		candidateCount += len(grouped)
	}

	// Fetch power states for filtering non-running VMs.
	// Wildcard resource-group fetchers use ListVirtualMachineStatuses, which
	// performs a subscription-wide ListAll with StatusOnly=true.
	// Non-wildcard resource-group fetchers skip power-state filtering because
	// Azure does not support StatusOnly for per-resource-group listings, and
	// per-VM Get calls for the full set would create O(N) amplification each poll cycle.
	// If no VMs remain after local filtering, power-state filtering is skipped.
	// If the bulk status fetch fails, power-state filtering is skipped for this cycle.
	var powerStates map[string]azure.PowerState
	powerFilterReason := ""
	switch {
	case !allowAllResourceGroups:
		powerFilterReason = powerFilterReasonNonWildcardResourceGroup
	case candidateCount == 0:
		powerFilterReason = powerFilterReasonNoCandidates
	default:
		powerStates, err = client.ListVirtualMachineStatuses(ctx)
		if err != nil {
			f.Logger.WarnContext(ctx,
				"Failed to fetch VM power states, skipping power state filter",
				"error", err,
			)
			powerStates = nil
			powerFilterReason = powerFilterReasonStatusFetchError
		}
	}

	if powerStates == nil {
		f.Logger.DebugContext(ctx,
			"Azure VM power-state filter not applied",
			"subscription_id", f.Subscription,
			"resource_group", f.ResourceGroup,
			"integration", f.Integration,
			"candidate_vms", candidateCount,
			"reason", powerFilterReason,
		)
	}

	// Filter non-running VMs from each batch group.
	// powerStates == nil means skip filtering (non-wildcard RG or
	// bulk call failure).
	fallbackLookups := 0
	fallbackFailures := 0
	fallbackLookupsSkipped := 0
	filteredNonRunning := 0
	for batchGroup, vms := range instByRegionAndRG {
		if powerStates == nil {
			continue
		}

		var running []*armcompute.VirtualMachine
		for _, vm := range vms {
			resourceID := azure.StringVal(vm.ID)
			vmName := azure.StringVal(vm.Name)
			rg := batchGroup.resourceGroup

			state, inMap := powerStates[resourceID]
			if !inMap {
				if fallbackLookups >= maxPowerStateFallbackLookupsPerFetch {
					fallbackLookupsSkipped++
					running = append(running, vm)
					continue
				}
				fallbackLookups++
				// VM missing from bulk response — targeted
				// per-VM fallback before deciding.
				fallbackState, getErr := client.GetVMPowerState(
					ctx, rg, vmName)
				if getErr != nil {
					fallbackFailures++
					// Fail-open: allow VM through to avoid
					// silently dropping reachable VMs.
					f.Logger.WarnContext(ctx,
						"VM missing from bulk power state response and per-VM lookup failed, allowing VM to proceed",
						"vm_name", vmName,
						"resource_id", resourceID,
						"error", getErr,
					)
					running = append(running, vm)
					continue
				}
				state = fallbackState
			}

			if state != azure.PowerStateRunning {
				filteredNonRunning++
				f.Logger.DebugContext(ctx,
					"Skipping Azure VM that is not running",
					"vm_name", vmName,
					"resource_id", resourceID,
					"power_state", string(state),
				)
				continue
			}

			running = append(running, vm)
		}
		instByRegionAndRG[batchGroup] = running
	}

	if powerStates != nil {
		if filteredNonRunning > 0 {
			f.Logger.InfoContext(ctx,
				"Skipping Azure VMs that are not running",
				"subscription_id", f.Subscription,
				"resource_group", f.ResourceGroup,
				"integration", f.Integration,
				"candidate_vms", candidateCount,
				"skipped_non_running", filteredNonRunning,
			)
		}
		if fallbackLookups > 0 {
			f.Logger.WarnContext(ctx,
				"Azure VMs missing from bulk power-state response, used per-VM fallback lookups",
				"subscription_id", f.Subscription,
				"resource_group", f.ResourceGroup,
				"integration", f.Integration,
				"candidate_vms", candidateCount,
				"bulk_status_entries", len(powerStates),
				"fallback_lookups", fallbackLookups,
				"fallback_failures", fallbackFailures,
			)
		}
		if fallbackLookupsSkipped > 0 {
			f.Logger.WarnContext(ctx,
				"Azure VM power-state fallback lookup limit reached, allowing remaining VMs to proceed without per-VM power check",
				"subscription_id", f.Subscription,
				"resource_group", f.ResourceGroup,
				"integration", f.Integration,
				"fallback_lookup_limit", maxPowerStateFallbackLookupsPerFetch,
				"fallback_lookups", fallbackLookups,
				"fallback_lookups_skipped", fallbackLookupsSkipped,
			)
		}

		f.Logger.DebugContext(ctx,
			"Azure VM power-state filtering summary",
			"subscription_id", f.Subscription,
			"resource_group", f.ResourceGroup,
			"integration", f.Integration,
			"candidate_vms", candidateCount,
			"bulk_status_entries", len(powerStates),
			"fallback_lookups", fallbackLookups,
			"fallback_failures", fallbackFailures,
			"fallback_lookups_skipped", fallbackLookupsSkipped,
			"filtered_non_running", filteredNonRunning,
		)
	}

	var instances []*AzureInstances
	for batchGroup, vms := range instByRegionAndRG {
		instances = append(instances, &AzureInstances{
			SubscriptionID:      f.Subscription,
			Region:              batchGroup.location,
			ResourceGroup:       batchGroup.resourceGroup,
			Instances:           vms,
			Integration:         f.Integration,
			InstallerParams:     f.InstallerParams,
			DiscoveryConfigName: f.DiscoveryConfigName,
		})
	}

	return instances, nil
}
