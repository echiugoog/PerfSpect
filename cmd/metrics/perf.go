package metrics

// Copyright (C) 2021-2025 Intel Corporation
// SPDX-License-Identifier: BSD-3-Clause

import (
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"perfspect/internal/script"
	"perfspect/internal/target"
	"perfspect/internal/util"
	"strings"
)

// extractPerf extracts the perf binary from the resources to the local temporary directory.
func extractPerf(myTarget target.Target, localTempDir string) (string, error) {
	// get the target architecture
	arch, err := myTarget.GetArchitecture()
	if err != nil {
		return "", fmt.Errorf("failed to get target architecture: %w", err)
	}
	// extract the perf binary
	return util.ExtractResource(script.Resources, path.Join("resources", arch, "perf"), localTempDir)
}

// getPerfPath determines the path to the `perf` binary for the given target.
// If the target is a local target, it uses the provided localPerfPath.
// If the target is remote, it checks if `perf` version 6.1 or later is available on the target.
// If available, it uses the `perf` binary on the target.
// If not available, it pushes the local `perf` binary to the target's temporary directory and uses that.
//
// Parameters:
// - myTarget: The target system where the `perf` binary is needed.
// - localPerfPath: The local path to the `perf` binary.
//
// Returns:
// - perfPath: The path to the `perf` binary on the target.
// - err: An error if any occurred during the process.
func getPerfPath(myTarget target.Target, localPerfPath string) (string, error) {
	if localPerfPath == "" {
		slog.Error("local perf path is empty, cannot determine perf path")
		return "", fmt.Errorf("local perf path is empty")
	}
	// local target
	if _, ok := myTarget.(*target.LocalTarget); ok {
		return localPerfPath, nil
	}
	// remote target
	targetTempDir := myTarget.GetTempDirectory()
	if targetTempDir == "" {
		slog.Error("target temporary directory is empty for remote target", slog.String("target", myTarget.GetName()))
		return "", fmt.Errorf("target temporary directory is empty for remote target %s", myTarget.GetName())
	}
	if err := myTarget.PushFile(localPerfPath, targetTempDir); err != nil {
		slog.Error("failed to push perf binary to remote directory", slog.String("error", err.Error()))
		return "", fmt.Errorf("failed to push perf binary to remote directory %s: %w", targetTempDir, err)
	}
	return path.Join(targetTempDir, "perf"), nil
}

// getPerfCommandArgs returns the command arguments for the 'perf stat' command
// based on the provided parameters.
//
// Parameters:
//   - pids: The process IDs for which to collect performance data. If flagScope is
//     set to "process", the data will be collected only for these processes.
//   - cgroups: The list of cgroups for which to collect performance data. If
//     flagScope is set to "cgroup", the data will be collected only for these cgroups.
//   - timeout: The timeout value in seconds. If flagScope is not set to "cgroup"
//     and timeout is not 0, the 'sleep' command will be added to the arguments
//     with the specified timeout value.
//   - eventGroups: The list of event groups to collect. Each event group is a
//     collection of events to be monitored.
//
// Returns:
// - args: The command arguments for the 'perf stat' command.
// - err: An error, if any.
func getPerfCommandArgs(pids []string, cgroups []string, timeout int, eventGroups []GroupDefinition, cpuRange string, md Metadata) (args []string, err error) {
	// -I: print interval in ms
	// -j: json formatted event output
	args = append(args, "stat", "-I", fmt.Sprintf("%d", flagPerfPrintInterval*1000), "-j")

	// -C: collect only for these cpus
	if cpuRange != "" {
		args = append(args, "-C", cpuRange) // collect only for these cpus
	}
	switch flagScope {
	case scopeSystem:
		args = append(args, "-a") // system-wide collection
		if flagGranularity == granularityCPU || flagGranularity == granularitySocket {
			args = append(args, "-A") // no aggregation
		}
	case scopeProcess:
		args = append(args, "-p", strings.Join(pids, ",")) // collect only for these processes
	case scopeCgroup:
		args = append(args, "--for-each-cgroup", strings.Join(cgroups, ",")) // collect only for these cgroups
	}

	// Check if any event group exceeds the number of available GP counters
	slog.Debug("checking event group sizes against available GP counters", "groups", len(eventGroups))
	for i, group := range eventGroups {
		hasCycleEvent := false
		hasInstructionsEvent := false
		hasTopDownSlotEvent := false
		hasRefCyclesEvent := false
		for _, event := range group {
			eventName := strings.ToLower(event.Name)
			if eventModifier := strings.Index(eventName, ":"); eventModifier > 0 {
				// strip any modifiers
				eventName = eventName[:eventModifier]
			}
			if eventName == "cycles" || eventName == "cpu-cycles" || eventName == "cpu_cycles" {
				slog.Debug("event group has cycles/cpu-cycles event", "group_index", i)
				hasCycleEvent = true
				continue
			}
			if eventName == "instructions" || eventName == "inst_retired" {
				slog.Debug("event group has instructions/inst_retired event", "group_index", i)
				hasInstructionsEvent = true
				continue
			}
			if eventName == "topdown.slots" {
				slog.Debug("event group has topdown.slots event", "group_index", i)
				hasTopDownSlotEvent = true
				continue
			}
			if eventName == "ref-cycles" {
				slog.Debug("event group has ref-cycles event", "group_index", i)
				hasRefCyclesEvent = true
				continue
			}
		}
		availableCounters := md.NumGeneralPurposeCounters
		if hasCycleEvent && md.SupportsFixedCycles {
			availableCounters++
		}
		if hasInstructionsEvent && md.SupportsFixedInstructions {
			availableCounters++
		}
		if hasTopDownSlotEvent && md.SupportsFixedTMA {
			availableCounters++
		}
		if hasRefCyclesEvent && md.SupportsRefCycles {
			availableCounters++
		}
		slog.Debug("perf event group size check", "group_index", i, "group_size", len(group), "available_counters", availableCounters)
		if len(group) > availableCounters {
			var eventList []string
			for _, event := range group {
				eventList = append(eventList, event.Name)
			}
			slog.Warn("Event group size exceeds available counters. Events likely not counted",
				"group_index", i,
				"event_count", len(group),
				"num_gp_counters", md.NumGeneralPurposeCounters,
				"available_counters", availableCounters,
				"has_cycle_event", hasCycleEvent,
				"has_ref_cycles_event", hasRefCyclesEvent,
				"has_instructions_event", hasInstructionsEvent,
				"has_topdown_slots_event", hasTopDownSlotEvent,
				"excess_count", len(group)-availableCounters,
				"events", strings.Join(eventList, ","))
		}
	}

	// -e: event groups to collect
	//args = append(args, "-e")
	//var groups []string
	for _, group := range eventGroups {
		var events []string
		for _, event := range group {
			events = append(events, event.Raw)
		}
		formattedGroup := fmt.Sprintf("'{%s}'", strings.Join(events, ","))
		args = append(args, "-e", formattedGroup)
		//groups = append(groups, fmt.Sprintf("{%s}", strings.Join(events, ",")))
	}
	//args = append(args, fmt.Sprintf("'%s'", strings.Join(groups, ",")))
	if len(argsApplication) > 0 {
		// add application args
		args = append(args, "--")
		args = append(args, argsApplication...)
	} else if flagScope != scopeCgroup && timeout != 0 {
		// add timeout
		args = append(args, "sleep", fmt.Sprintf("%d", timeout))
	}
	return
}

// getPerfCommand is responsible for assembling the command that will be
// executed to collect event data
func getPerfCommand(perfPath string, eventGroups []GroupDefinition, pids []string, cids []string, cpuRange string, md Metadata) (*exec.Cmd, error) {
	var duration int
	switch flagScope {
	case scopeSystem:
		duration = flagDuration
	case scopeProcess:
		if flagDuration > 0 {
			duration = flagDuration
		} else if len(flagPidList) == 0 { // don't refresh if PIDs are specified
			duration = flagRefresh // refresh hot processes every flagRefresh seconds
		}
	case scopeCgroup:
		duration = 0
	}

	args, err := getPerfCommandArgs(pids, cids, duration, eventGroups, cpuRange, md)
	if err != nil {
		err = fmt.Errorf("failed to assemble perf args: %v", err)
		return nil, err
	}
	perfCommand := exec.Command(perfPath, args...) // #nosec G204 // nosemgrep
	return perfCommand, nil
}
