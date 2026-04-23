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

// powerFilterSkipReason names the reasons power-state filtering was not applied for a given poll cycle.
// The empty value means "filtering was applied" (no skip). Used in fetchNonRunningStates return values and the
// "reason" log attribute.
type powerFilterSkipReason string

const (
	powerFilterReasonNoCandidates     powerFilterSkipReason = "no_candidates"
	powerFilterReasonStatusFetchError powerFilterSkipReason = "status_fetch_error"
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

// GetInstances fetches all Azure virtual machines matching configured filters,
// drops known non-Linux VMs, and applies best-effort power-state filtering.
//
// OS filtering: VMs with a known non-Linux OS type (e.g. Windows) are excluded;
// VMs with unknown OS type pass through to avoid silently dropping Linux VMs
// whose metadata is missing.
//
// Power-state filtering is best-effort: candidate VMs pass through unfiltered
// when the bulk power-state fetch fails. Only VMs positively identified as
// non-running are excluded.
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

	vms = f.filterNonLinux(ctx, vms)
	instByRegionAndRG, candidateCount := f.groupCandidates(ctx, vms)

	// Fetch known non-running VM states, filter matching candidates in place, log the summary. ARM RBAC-filters
	// the subscription-wide bulk response to VMs the caller's identity can see, so both wildcard and non-wildcard
	// resource-group fetchers issue the bulk call and reconcile that non-running set against their local candidates.
	// If the fetch fails, power-state filtering is skipped and the matcher fails open.
	nonRunning, skipReason := f.fetchNonRunningStates(ctx, client, candidateCount)
	if nonRunning == nil {
		f.Logger.DebugContext(ctx,
			"Azure VM power-state filter not applied",
			"subscription_id", f.Subscription,
			"resource_group", f.ResourceGroup,
			"integration", f.Integration,
			"candidate_vms", candidateCount,
			"reason", skipReason,
		)
	} else {
		filteredNonRunning := f.filterNonRunningInPlace(ctx, instByRegionAndRG, nonRunning)
		f.logPowerFilterSummary(ctx, candidateCount, len(nonRunning), filteredNonRunning)
	}

	// fetchNonRunningStates swallows ARM errors as "skip filter, fail open."
	// Cancellation shouldn't be treated that way: handing an unfiltered set to the installer during
	// shutdown is incorrect API behavior. Propagate it instead.
	if err := ctx.Err(); err != nil {
		return nil, trace.Wrap(err)
	}

	var instances []*AzureInstances
	for batchGroup, vms := range instByRegionAndRG {
		// Skip buckets whose VMs were all filtered out. Emitting empty AzureInstances adds no-op work downstream
		// (a channel send, an install cycle that early-returns, a status-panel entry with statusFound: 0).
		if len(vms) == 0 {
			continue
		}
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

// filterNonLinux drops VMs whose reported OS type is a known non-Linux type (e.g. Windows).
// VMs with unknown OS type pass through, to avoid silently dropping legitimate Linux VMs whose metadata
// is missing. Logs one info-level summary when any VMs are skipped, plus a debug entry per skipped VM.
func (f *azureInstanceFetcher) filterNonLinux(
	ctx context.Context,
	vms []*armcompute.VirtualMachine,
) []*armcompute.VirtualMachine {
	filtered := azure.FilterLinuxVMs(vms)
	if len(filtered.Skipped) == 0 {
		return filtered.Linux
	}
	f.Logger.InfoContext(ctx,
		"Skipping Azure VMs with non-Linux OS type",
		"subscription_id", f.Subscription,
		"resource_group", f.ResourceGroup,
		"integration", f.Integration,
		"candidate_vms", len(filtered.Skipped)+len(filtered.Linux),
		"skipped", len(filtered.Skipped),
		"kept", len(filtered.Linux),
	)
	for _, skipped := range filtered.Skipped {
		f.Logger.DebugContext(ctx,
			"Skipping Azure VM with non-Linux OS type",
			"vm_name", azure.StringVal(skipped.VM.Name),
			"resource_id", azure.StringVal(skipped.VM.ID),
			"os_type", skipped.OSType,
		)
	}
	return filtered.Linux
}

// groupCandidates filters the given VMs by the fetcher's configured region and tag matchers, resolves
// each surviving VM's resource group (either the fetcher's configured group or, for wildcard matchers,
// parsed from the VM's resource ID), and buckets the result by (resourceGroup, location). VMs whose
// resource ID can't be parsed under a wildcard RG matcher are logged and skipped.
func (f *azureInstanceFetcher) groupCandidates(
	ctx context.Context,
	vms []*armcompute.VirtualMachine,
) (map[resourceGroupLocation][]*armcompute.VirtualMachine, int) {
	byRG := make(map[resourceGroupLocation][]*armcompute.VirtualMachine)
	candidateCount := 0
	allowAllLocations := slices.Contains(f.Regions, types.Wildcard)
	allowAllResourceGroups := f.ResourceGroup == types.Wildcard

	for _, vm := range vms {
		// Empty vm.ID has no map-lookup key downstream (nonRunning[""] would collide every empty-ID VM) and
		// can't be parsed under wildcard RG. Skip with a single Warn rather than let it cascade.
		resourceID := azure.StringVal(vm.ID)
		if resourceID == "" {
			f.Logger.WarnContext(ctx, "Skipping Azure VM with empty resource ID",
				"subscription_id", f.Subscription,
				"integration", f.Integration,
			)
			continue
		}

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
			resourceMetadata, err := arm.ParseResourceID(resourceID)
			if err != nil {
				f.Logger.WarnContext(ctx, "Skipping Teleport installation on Azure VM - failed to infer resource group from vm id",
					"subscription_id", f.Subscription,
					"vm_id", azure.VMID(vm),
					"resource_id", resourceID,
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
		byRG[batchGroup] = append(byRG[batchGroup], vm)
		candidateCount++
	}
	return byRG, candidateCount
}

// fetchNonRunningStates returns the subscription-wide non-running power-state map for this fetcher's
// subscription, or nil with a non-empty skipReason when power-state filtering should be skipped for this
// cycle (no candidates, or bulk fetch failed). skipReason is one of the powerFilterReason* constants.
func (f *azureInstanceFetcher) fetchNonRunningStates(
	ctx context.Context,
	client azure.VirtualMachinesClient,
	candidateCount int,
) (map[string]azure.PowerState, powerFilterSkipReason) {
	if candidateCount == 0 {
		return nil, powerFilterReasonNoCandidates
	}
	nonRunning, err := client.ListNonRunningVirtualMachineStates(ctx)
	if err != nil {
		// Intentionally swallowed: returning the error would halt discovery for this subscription for the
		// entire poll cycle. Instead, skip power-state filtering (fail open) so candidates still flow through to enrollment.
		//
		// AccessDenied gets a dedicated, actionable message so operators can
		// tell a persistent misconfiguration apart from a transient ARM error.
		if trace.IsAccessDenied(err) {
			f.Logger.WarnContext(ctx,
				"Identity lacks permission to fetch VM power states, skipping power state filter — grant Reader on the subscription to enable filtering",
				"subscription_id", f.Subscription,
				"resource_group", f.ResourceGroup,
				"integration", f.Integration,
				"error", err,
			)
			return nil, powerFilterReasonStatusFetchError
		}
		f.Logger.WarnContext(ctx,
			"Failed to fetch VM power states, skipping power state filter",
			"subscription_id", f.Subscription,
			"resource_group", f.ResourceGroup,
			"integration", f.Integration,
			"error", err,
		)
		return nil, powerFilterReasonStatusFetchError
	}
	return nonRunning, ""
}

// filterNonRunningInPlace removes non-running VMs from the grouped candidate map in place. Any VM
// absent from the non-running map is treated as running or indeterminate and passes through.
func (f *azureInstanceFetcher) filterNonRunningInPlace(
	ctx context.Context,
	byRG map[resourceGroupLocation][]*armcompute.VirtualMachine,
	nonRunning map[string]azure.PowerState,
) int {
	filteredNonRunning := 0
	for batchGroup, vms := range byRG {
		byRG[batchGroup] = slices.DeleteFunc(vms, func(vm *armcompute.VirtualMachine) bool {
			resourceID := azure.StringVal(vm.ID)
			state, isNonRunning := nonRunning[resourceID]
			if !isNonRunning {
				return false
			}

			filteredNonRunning++
			f.Logger.DebugContext(ctx,
				"Skipping Azure VM that is not running",
				"vm_name", azure.StringVal(vm.Name),
				"resource_id", resourceID,
				"power_state", string(state),
			)
			return true
		})
	}
	return filteredNonRunning
}

// logPowerFilterSummary emits per-iteration summary logs after power-state filtering has run:
// info when non-running VMs were skipped, plus a debug-level summary.
func (f *azureInstanceFetcher) logPowerFilterSummary(
	ctx context.Context,
	candidateCount, nonRunningEntries, filteredNonRunning int,
) {
	if filteredNonRunning > 0 {
		f.Logger.InfoContext(ctx,
			"Skipping Azure VMs that are not running",
			"subscription_id", f.Subscription,
			"resource_group", f.ResourceGroup,
			"integration", f.Integration,
			"candidate_vms", candidateCount,
			"skipped", filteredNonRunning,
			"kept", candidateCount-filteredNonRunning,
		)
	}
	f.Logger.DebugContext(ctx,
		"Azure VM power-state filtering summary",
		"subscription_id", f.Subscription,
		"resource_group", f.ResourceGroup,
		"integration", f.Integration,
		"candidate_vms", candidateCount,
		"non_running_entries", nonRunningEntries,
		"filtered_non_running", filteredNonRunning,
	)
}
