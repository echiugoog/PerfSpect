package metrics

// Copyright (C) 2021-2025 Intel Corporation
// SPDX-License-Identifier: BSD-3-Clause

// helper functions for parsing and interpreting the architecture-specific perf event definition files

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
)

// EventDefinition represents a single perf event
type EventDefinition struct {
	Raw         string
	Name        string
	Device      string
	Description string
}

// ARM64Event represents the structure of an event in the ARM PMU event JSON files (e.g. https://github.com/torvalds/linux/tree/master/tools/perf/pmu-events/arch/arm64/arm).
type ARM64Event struct {
	ArchStdEvent      string `json:"ArchStdEvent"`
	PublicDescription string `json:"PublicDescription"`
	// Add other fields if present in JSON and needed
}

// GroupDefinition represents a group of perf events
type GroupDefinition []EventDefinition

// LoadEventGroups reads the events defined in the architecture specific event definition file, then
// expands them to include the per-device uncore events
func LoadEventGroups(eventDefinitionOverridePath string, metadata Metadata) (groups []GroupDefinition, uncollectableEvents []string, err error) {
	if metadata.Architecture == "arm64" || metadata.Architecture == "aarch64" {
		return LoadArmEventGroups(eventDefinitionOverridePath, metadata)
	}
	var file fs.File
	if eventDefinitionOverridePath != "" {
		if file, err = os.Open(eventDefinitionOverridePath); err != nil {
			slog.Error("Failed to open event definition override file", slog.String("path", eventDefinitionOverridePath), slog.Any("error", err))
			return
		}
	} else {
		uarch := strings.ToLower(strings.Split(metadata.Microarchitecture, "_")[0])
		uarch = strings.Split(uarch, " ")[0]
		// use alternate events/metrics when TMA fixed counters are not supported
		alternate := ""
		if (uarch == "icx" || uarch == "spr" || uarch == "emr") && !metadata.SupportsFixedTMA { // AWS VM instances
			alternate = "_nofixedtma"
		}
		eventFileName := fmt.Sprintf("%s%s.txt", uarch, alternate)
		if file, err = resources.Open(filepath.Join("resources", "events", metadata.Architecture, metadata.Vendor, eventFileName)); err != nil {
			slog.Error("Failed to open event definition file", slog.String("path", eventFileName), slog.Any("error", err))
			return
		}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	uncollectable := mapset.NewSet[string]()
	var group GroupDefinition
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var event EventDefinition
		if event, err = parseEventDefinition(line[:len(line)-1]); err != nil {
			slog.Error("Failed to parse event definition", slog.String("line", line), slog.Any("error", err))
			return
		}
		// abbreviate the event name to shorten the eventual perf stat command line
		event.Name = abbreviateEventName(event.Name)
		event.Raw = abbreviateEventName(event.Raw)
		if isCollectableEvent(event, metadata) {
			group = append(group, event)
		} else {
			uncollectable.Add(event.Name)
		}
		if line[len(line)-1] == ';' {
			// end of group detected
			if len(group) > 0 {
				groups = append(groups, group)
			} else {
				slog.Warn("No collectable events in group", slog.String("ending", line))
			}
			group = GroupDefinition{} // clear the list
		}
	}
	if err = scanner.Err(); err != nil {
		slog.Error("Error reading event definition file", slog.Any("error", err))
		return
	}
	uncollectableEvents = uncollectable.ToSlice()
	// expand uncore groups for all uncore devices
	groups, err = expandUncoreGroups(groups, metadata)

	if uncollectable.Cardinality() != 0 {
		slog.Warn("Events not collectable on target", slog.String("events", uncollectable.String()))
	}
	return
}

func LoadArmEventGroups(eventDefinitionOverridePath string, metadata Metadata) (groups []GroupDefinition, uncollectableEvents []string, err error) {
	var eventFiles []fs.DirEntry
	var eventDirPath string

	if eventDefinitionOverridePath != "" {
		// If override path is a directory, use it; otherwise, it's an unsupported override for ARM.
		// For simplicity, let's assume override path can be a directory of JSONs.
		// A more robust solution would check if it's a file or dir.
		fi, statErr := os.Stat(eventDefinitionOverridePath)
		if statErr != nil {
			err = fmt.Errorf("failed to stat eventDefinitionOverridePath '%s': %w", eventDefinitionOverridePath, statErr)
			return
		}
		if !fi.IsDir() {
			// If it's a single file, this logic path is not designed for it with ARM.
			// For now, we can fall back to trying to parse it as a legacy file, or error out.
			// Let's error, as ARM expects a directory of JSONs.
			err = fmt.Errorf("ARM64 eventDefinitionOverridePath '%s' is not a directory", eventDefinitionOverridePath)
			return

		}
		eventDirPath = eventDefinitionOverridePath
		eventFiles, err = os.ReadDir(eventDirPath)
		if err != nil {
			err = fmt.Errorf("failed to read override event directory '%s': %w", eventDirPath, err)
			return
		}
	} else {
		var microarchitectureVariant string
		if microarchitectureVariant, err = lookupArmVariant(metadata); err != nil {
			err = fmt.Errorf("failed to lookup ARM variant: %w", err)
			slog.Error("Failed to lookup ARM variant", slog.Any("error", err))
			return
		}

		eventDirPath = filepath.Join("resources", "events", metadata.Architecture, microarchitectureVariant)
		eventFiles, err = resources.ReadDir(eventDirPath)
		if err != nil {
			err = fmt.Errorf("failed to read ARM event directory '%s': %w", eventDirPath, err)
			// Fallback or specific error handling if directory doesn't exist
			// For now, if the directory specific to microarchitecture doesn't exist, return error.
			slog.Error("ARM event directory not found", slog.String("path", eventDirPath), slog.Any("error", err))
			return
		}
	}

	uncollectable := mapset.NewSet[string]()
	for _, fileEntry := range eventFiles {
		slog.Debug("fileEntry", slog.String("name", fileEntry.Name()))
		if !fileEntry.IsDir() && strings.HasSuffix(strings.ToLower(fileEntry.Name()), ".json") {
			var filePath string
			var fileData []byte
			if eventDefinitionOverridePath != "" { // Reading from an override directory
				filePath = filepath.Join(eventDirPath, fileEntry.Name())
				fileData, err = os.ReadFile(filePath)
			} else { // Reading from embedded resources
				filePath = filepath.Join(eventDirPath, fileEntry.Name())
				fileData, err = resources.ReadFile(filePath)
			}

			if err != nil {
				slog.Warn("Failed to read ARM event file", slog.String("file", filePath), slog.Any("error", err))
				continue
			}

			var armEvents []ARM64Event
			if err = json.Unmarshal(fileData, &armEvents); err != nil {
				slog.Warn("Failed to parse ARM event file", slog.String("file", filePath), slog.Any("error", err))
				continue
			}

			var currentGroup GroupDefinition
			for _, armEvent := range armEvents {
				slog.Debug("ARM event definition", slog.Any("event", armEvent))
				event := EventDefinition{
					Name:        armEvent.ArchStdEvent,
					Raw:         armEvent.ArchStdEvent, // Or synthesize e.g., "arm64/" + armEvent.ArchStdEvent
					Device:      "cpu",                 // assume all CPU events for now
					Description: armEvent.PublicDescription,
				}
				if isCollectableEvent(event, metadata) {
					currentGroup = append(currentGroup, event)
				} else {
					slog.Debug("Event not collectable on target", slog.String("name", event.Name))
					uncollectable.Add(event.Name)
				}
			}
			if len(currentGroup) > 0 {
				groups = append(groups, currentGroup)
			} else {
				slog.Warn("No collectable ARM events in file", slog.String("file", filePath))
			}
		}
	}
	return
}

func lookupArmVariant(metadata Metadata) (string, error) {
	if metadata.Microarchitecture == "Neoverse V2" {
		return "neoverse-n2-v2", nil
	}
	return "", fmt.Errorf("unknown ARM variant: %s", metadata.Microarchitecture)
}

// abbreviateEventName replaces long event names with abbreviations to reduce the length of the perf command.
// focus is on uncore events because they are repeated for each uncore device
func abbreviateEventName(event string) string {
	// Abbreviations must be unique and in order. And, if replacing UNC_*, the abbreviation must begin with "UNC" because this is how we identify uncore events when collapsing them.
	var abbreviations = [][]string{
		{"UNC_CHA_TOR_INSERTS", "UNCCTI"},
		{"UNC_CHA_TOR_OCCUPANCY", "UNCCTO"},
		{"UNC_CHA_CLOCKTICKS", "UNCCCT"},
		{"UNC_M_CAS_COUNT_SCH", "UNCMCC"},
		{"IA_MISS_DRD_REMOTE", "IMDR"},
		{"IA_MISS_DRD_LOCAL", "IMDL"},
		{"IA_MISS_LLCPREFDATA", "IMLP"},
		{"IA_MISS_LLCPREFRFO", "IMLR"},
		{"IA_MISS_DRD_PREF_LOCAL", "IMDPL"},
		{"IA_MISS_DRD_PREF_REMOTE", "IMDRP"},
		{"IA_MISS_CRD_PREF", "IMCP"},
		{"IA_MISS_RFO_PREF", "IMRP"},
		{"IA_MISS_RFO", "IMRF"},
		{"IA_MISS_CRD", "IMC"},
		{"IA_MISS_DRD", "IMD"},
		{"IO_PCIRDCUR", "IPCI"},
		{"IO_ITOMCACHENEAR", "IITN"},
		{"IO_ITOM", "IITO"},
		{"IMD_OPT", "IMDO"},
	}
	// if an abbreviation key is found in the event, replace the matching portion of the event with the abbreviation
	for _, abbr := range abbreviations {
		event = strings.Replace(event, abbr[0], abbr[1], -1)
	}
	return event
}

// isCollectableEvent confirms if given event can be collected on the platform
func isCollectableEvent(event EventDefinition, metadata Metadata) bool {
	// fixed-counter TMA
	if !metadata.SupportsFixedTMA && (event.Name == "TOPDOWN.SLOTS" || strings.HasPrefix(event.Name, "PERF_METRICS.")) {
		slog.Debug("Fixed counter TMA not supported on target", slog.String("event", event.Name))
		return false
	}
	// PEBS events (not supported on GCP c4 VMs)
	pebsEventNames := []string{"INT_MISC.UNKNOWN_BRANCH_CYCLES", "UOPS_RETIRED.MS"}
	if !metadata.SupportsPEBS && slices.Contains(pebsEventNames, event.Name) {
		slog.Debug("PEBS events not supported on target", slog.String("event", event.Name))
		return false
	}
	// short-circuit for cpu events that aren't off-core response events
	if event.Device == "cpu" && !(strings.HasPrefix(event.Name, "OCR") || strings.HasPrefix(event.Name, "OFFCORE_REQUESTS_OUTSTANDING")) {
		return true
	}
	// off-core response events
	if event.Device == "cpu" && (strings.HasPrefix(event.Name, "OCR") || strings.HasPrefix(event.Name, "OFFCORE_REQUESTS_OUTSTANDING")) {
		if !(metadata.SupportsOCR && metadata.SupportsUncore) {
			slog.Debug("Off-core response events not supported on target", slog.String("event", event.Name))
			return false
		} else if flagScope == scopeProcess || flagScope == scopeCgroup {
			slog.Debug("Off-core response events not supported in process or cgroup scope", slog.String("event", event.Name))
			return false
		}
		return true
	}
	// uncore events
	if !metadata.SupportsUncore && strings.HasPrefix(event.Name, "UNC") {
		slog.Debug("Uncore events not supported on target", slog.String("event", event.Name))
		return false
	}
	// exclude uncore events when
	// - their corresponding device is not found
	// - not in system-wide collection scope
	if event.Device != "cpu" && event.Device != "" {
		if flagScope == scopeProcess || flagScope == scopeCgroup {
			slog.Debug("Uncore events not supported in process or cgroup scope", slog.String("event", event.Name))
			return false
		}
		deviceExists := false
		for uncoreDeviceName := range metadata.UncoreDeviceIDs {
			if event.Device == uncoreDeviceName {
				deviceExists = true
				break
			}
		}
		if !deviceExists {
			slog.Debug("Uncore device not found", slog.String("device", event.Device))
			return false
		} else if !strings.Contains(event.Raw, "umask") && !strings.Contains(event.Raw, "event") {
			slog.Debug("Uncore event missing umask or event", slog.String("event", event.Name))
			return false
		}
		return true
	}
	// if we got this far, event.Device is empty
	// is ref-cycles supported?
	if !metadata.SupportsRefCycles && strings.Contains(event.Name, "ref-cycles") {
		slog.Debug("ref-cycles not supported on target", slog.String("event", event.Name))
		return false
	}
	// no cstate and power events when collecting at process or cgroup scope
	if (flagScope == scopeProcess || flagScope == scopeCgroup) &&
		(strings.Contains(event.Name, "cstate_") || strings.Contains(event.Name, "power/energy")) {
		slog.Debug("Cstate and power events not supported in process or cgroup scope", slog.String("event", event.Name))
		return false
	}
	// finally, if it isn't in the perf list output, it isn't collectable
	name := strings.Split(event.Name, ":")[0]
	if !strings.Contains(metadata.PerfSupportedEvents, name) {
		slog.Debug("Event not supported by perf", slog.String("event", name))
		return false
	}
	return true
}

// parseEventDefinition parses one line from the event definition file into a representative structure
func parseEventDefinition(line string) (eventDef EventDefinition, err error) {
	eventDef.Raw = line
	fields := strings.Split(line, ",")
	if len(fields) == 1 {
		eventDef.Name = fields[0]
	} else if len(fields) > 1 {
		nameField := fields[len(fields)-1]
		if nameField[:5] != "name=" {
			err = fmt.Errorf("unrecognized event format, name field not found: %s", line)
			return
		}
		eventDef.Name = nameField[6 : len(nameField)-2]
		eventDef.Device = strings.Split(fields[0], "/")[0]
	} else {
		err = fmt.Errorf("unrecognized event format: %s", line)
		return
	}
	return
}

// expandUncoreGroup expands a perf event group into a list of groups where each group is
// associated with an uncore device
func expandUncoreGroup(group GroupDefinition, ids []int, re *regexp.Regexp, vendor string) (groups []GroupDefinition, err error) {
	for _, deviceID := range ids {
		var newGroup GroupDefinition
		for _, event := range group {
			match := re.FindStringSubmatch(event.Raw)
			if len(match) == 0 {
				err = fmt.Errorf("unexpected raw event format: %s", event.Raw)
				return
			}
			var newEvent EventDefinition
			if vendor == "AuthenticAMD" {
				newEvent.Name = match[4]
				newEvent.Raw = fmt.Sprintf("amd_%s/event=%s,umask=%s,name='%s'/", match[1], match[2], match[3], newEvent.Name)
			} else {
				newEvent.Name = fmt.Sprintf("%s.%d", match[4], deviceID)
				newEvent.Raw = fmt.Sprintf("uncore_%s_%d/event=%s,umask=%s,name='%s'/", match[1], deviceID, match[2], match[3], newEvent.Name)
			}
			newEvent.Device = event.Device
			newGroup = append(newGroup, newEvent)
		}
		groups = append(groups, newGroup)
	}
	return
}

// expandUncoreGroups expands groups with uncore events to include events for all uncore devices
// assumes that uncore device events are in their own groups, not mixed with other device types
func expandUncoreGroups(groups []GroupDefinition, metadata Metadata) (expandedGroups []GroupDefinition, err error) {
	// example 1: cha/event=0x35,umask=0xc80ffe01,name='UNC_CHA_TOR_INSERTS.IA_MISS_CRD'/,
	// expand to: uncore_cha_0/event=0x35,umask=0xc80ffe01,name='UNC_CHA_TOR_INSERTS.IA_MISS_CRD.0'/,
	// example 2: cha/event=0x36,umask=0x21,config1=0x4043300000000,name='UNC_CHA_TOR_OCCUPANCY.IA_MISS.0x40433'/
	// expand to: uncore_cha_0/event=0x36,umask=0x21,config1=0x4043300000000,name='UNC_CHA_TOR_OCCUPANCY.IA_MISS.0x40433'/
	re := regexp.MustCompile(`(\w+)/event=(0x[0-9,a-f,A-F]+),umask=(0x[0-9,a-f,A-F]+.*),name='(.*)'`)
	var deviceTypes []string
	for deviceType := range metadata.UncoreDeviceIDs {
		deviceTypes = append(deviceTypes, deviceType)
	}
	for _, group := range groups {
		device := group[0].Device
		if slices.Contains(deviceTypes, device) {
			var newGroups []GroupDefinition
			if len(metadata.UncoreDeviceIDs[device]) == 0 {
				slog.Warn("No uncore devices found", slog.String("type", device))
				continue
			}
			if newGroups, err = expandUncoreGroup(group, metadata.UncoreDeviceIDs[device], re, metadata.Vendor); err != nil {
				return
			}
			expandedGroups = append(expandedGroups, newGroups...)
		} else {
			expandedGroups = append(expandedGroups, group)
		}
	}
	return
}
