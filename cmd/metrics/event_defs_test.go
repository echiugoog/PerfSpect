package metrics

import (
	_ "embed" // Required for go:embed
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// var logOpts slog.HandlerOptions
	// logOpts.Level = slog.LevelDebug
	// handler := slog.NewJSONHandler(os.Stdout, &logOpts)
	// slog.SetDefault(slog.New(handler))
	m.Run()
}

// Helper function to create a basic Metadata struct for testing ARM64.
func newArmMetadata(microarch string) Metadata {
	return Metadata{
		Architecture:      "aarch64", // Consistent with uname -m
		Vendor:            "ARM",     // Generic vendor, specific ID not used in ARM path
		Microarchitecture: microarch,
		// Populate PerfSupportedEvents with all events we expect to find and test.
		// This ensures they are not filtered out by isCollectableEvent.
		PerfSupportedEvents: "CPU_CYCLES,CNT_CYCLES,INST_RETIRED,SW_INCR,CID_WRITE_RETIRED,TTBR_WRITE_RETIRED,BR_RETIRED,BR_MIS_PRED_RETIRED,OP_RETIRED,SVE_INST_SPEC,SVE_PRED_SPEC,SVE_PRED_EMPTY_SPEC,SVE_PRED_FULL_SPEC,SVE_PRED_PARTIAL_SPEC,SVE_PRED_NOT_FULL_SPEC,SVE_LDFF_SPEC,SVE_LDFF_FAULT_SPEC,ASE_SVE_INT8_SPEC,ASE_SVE_INT16_SPEC,ASE_SVE_INT32_SPEC,ASE_SVE_INT64_SPEC,STALL_FRONTEND,STALL_BACKEND,STALL,STALL_SLOT_BACKEND,STALL_SLOT_FRONTEND,STALL_SLOT,STALL_BACKEND_MEM",
		UncoreDeviceIDs:     make(map[string][]int), // Initialize to avoid nil pointer
		SupportsUncore:      false,                  // ARM events are core events
		SupportsFixedTMA:    false,                  // Not relevant for these ARM events
		SupportsPEBS:        false,                  // Not relevant
		SupportsOCR:         false,                  // Not relevant
	}
}

func TestLoadEventGroups_ARM64_NeoverseV2(t *testing.T) {
	metadata := newArmMetadata("Neoverse V2")
	// The event files are expected at "resources/events/aarch64/neoverse-n2-v2/*.json"

	groups, uncollectableEvents, err := LoadEventGroups("", metadata)
	if err != nil {
		t.Fatalf("LoadEventGroups failed: %v", err)
	}

	if len(groups) == 0 {
		t.Fatal("Expected event groups to be loaded, but got an empty list.")
	}

	// Define a map of expected events to their descriptions for easier checking
	// Using strings.HasPrefix for description check for robustness.
	expectedEvents := map[string]string{
		"CPU_CYCLES":          "Counts CPU clock cycles (not timer cycles).",
		"INST_RETIRED":        "Counts instructions that have been architecturally executed.",
		"SVE_INST_SPEC":       "Counts speculatively executed operations that are SVE operations.",
		"STALL_FRONTEND":      "Counts cycles when frontend could not send any micro-operations to the rename stage",
		"CNT_CYCLES":          "Increments at a constant frequency equal to the rate of increment of the System Counter, CNTPCT_EL0.",
		"SW_INCR":             "Counts software writes to the PMSWINC_EL0 (software PMU increment) register.",
		"BR_MIS_PRED_RETIRED": "Counts branches counted by BR_RETIRED which were mispredicted and caused a pipeline flush.",
		"ASE_SVE_INT64_SPEC":  "Counts speculatively executed Advanced SIMD or SVE integer operations with the largest data type a 64-bit integer.",
		"STALL_BACKEND_MEM":   "Counts cycles when the backend is stalled because there is a pending demand load request in progress in the last level core cache.",
	}

	foundEvents := make(map[string]EventDefinition)
	for _, group := range groups {
		for _, event := range group {
			foundEvents[event.Name] = event
		}
	}

	for eventName, expectedDescPrefix := range expectedEvents {
		event, ok := foundEvents[eventName]
		if !ok {
			t.Errorf("Expected event '%s' not found in loaded groups.", eventName)
			continue
		}
		if event.Raw != eventName {
			t.Errorf("For event '%s', expected Raw to be '%s', got '%s'", eventName, eventName, event.Raw)
		}
		if event.Device != "cpu" {
			t.Errorf("For event '%s', expected Device to be 'cpu', got '%s'", eventName, event.Device)
		}
		if !strings.HasPrefix(event.Description, expectedDescPrefix) {
			t.Errorf("For event '%s', description prefix mismatch.\nExpected prefix: '%s'\nGot:             '%s'", eventName, expectedDescPrefix, event.Description)
		}
	}

	// Check for specific events from each file to infer files were loaded
	eventsFromGeneral := []string{"CPU_CYCLES", "CNT_CYCLES"}
	eventsFromRetired := []string{"INST_RETIRED", "SW_INCR"}
	eventsFromSVE := []string{"SVE_INST_SPEC", "ASE_SVE_INT64_SPEC"}
	eventsFromStall := []string{"STALL_FRONTEND", "STALL_BACKEND_MEM"}

	checkEventPresence := func(eventList []string, fileName string) {
		for _, eventName := range eventList {
			if _, ok := foundEvents[eventName]; !ok {
				t.Errorf("Expected event '%s' from %s not found.", eventName, fileName)
			}
		}
	}

	checkEventPresence(eventsFromGeneral, "general.json")
	checkEventPresence(eventsFromRetired, "retired.json")
	checkEventPresence(eventsFromSVE, "sve.json")
	checkEventPresence(eventsFromStall, "stall.json")

	if len(uncollectableEvents) > 0 {
		// This test provides all expected events in PerfSupportedEvents, so this list should ideally be empty.
		// If not, it indicates an issue with isCollectableEvent or PerfSupportedEvents list.
		t.Errorf("Warning: Some events were unexpectedly marked uncollectable: %v. Check PerfSupportedEvents in metadata and isCollectableEvent logic.", uncollectableEvents)
	}
}
