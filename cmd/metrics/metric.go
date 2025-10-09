package metrics

// Copyright (C) 2021-2025 Intel Corporation
// SPDX-License-Identifier: BSD-3-Clause

// metric generation type defintions and helper functions

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"

	mapset "github.com/deckarep/golang-set/v2"
)

// Metric represents a metric (name, value) derived from perf events
type Metric struct {
	Name  string
	Value float64
}

// MetricFrame represents the metrics values and associated metadata
type MetricFrame struct {
	Metrics   []Metric
	Timestamp float64
	Socket    string
	CPU       string
	Cgroup    string
	PID       string
	Cmd       string
}

// ProcessEvents is responsible for producing metrics from raw perf events
func ProcessEvents(perfEvents [][]byte, eventGroupDefinitions []GroupDefinition, metricDefinitions []MetricDefinition, processes []Process, previousTimestamp float64, metadata Metadata) (metricFrames []MetricFrame, timeStamp float64, err error) {
	var eventFrames []EventFrame
	if eventFrames, err = GetEventFrames(perfEvents, eventGroupDefinitions, flagScope, flagGranularity, metadata); err != nil { // arrange the events into groups
		err = fmt.Errorf("failed to put perf events into groups: %v", err)
		return
	}

	// Topologically sort the metric definitions to ensure correct evaluation order.
	sortedMetricDefs, err := topologicalSort(metricDefinitions)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to sort metric definitions: %w", err)
	}

	metricFrames = make([]MetricFrame, 0, len(eventFrames))
	for _, eventFrame := range eventFrames {
		timeStamp = eventFrame.Timestamp
		var metricFrame MetricFrame
		metricFrame.Metrics = make([]Metric, 0, len(sortedMetricDefs))
		metricFrame.Timestamp = eventFrame.Timestamp
		metricFrame.Socket = eventFrame.Socket
		metricFrame.CPU = eventFrame.CPU
		metricFrame.Cgroup = eventFrame.Cgroup
		var pidList []string
		var cmdList []string
		for _, process := range processes {
			pidList = append(pidList, process.pid)
			cmdList = append(cmdList, process.cmd)
		}
		metricFrame.PID = strings.Join(pidList, ",")
		metricFrame.Cmd = strings.Join(cmdList, ",")

		// A map to store the computed metric values for the current frame.
		computedMetrics := make(map[string]float64)

		// produce metrics from event groups
		allMetricNames := mapset.NewSet[string]()
		for _, m := range sortedMetricDefs {
			allMetricNames.Add(m.Name)
		}
		for _, metricDef := range sortedMetricDefs {
			metric := Metric{Name: metricDef.Name, Value: math.NaN()}
			var variables map[string]any
			if variables, err = getExpressionVariableValues(metricDef, eventFrame, previousTimestamp, metadata, computedMetrics, allMetricNames); err != nil {
				slog.Debug("failed to get expression variable values", slog.String("metric", metricDef.Name), slog.String("error", err.Error()))
				err = nil
			} else {
				var result any
				if result, err = evaluateExpression(metricDef, variables); err != nil {
					slog.Debug("failed to evaluate expression", slog.String("error", err.Error()))
					err = nil
				} else {
					metric.Value = result.(float64)
					computedMetrics[metric.Name] = metric.Value
				}
			}
			metricFrame.Metrics = append(metricFrame.Metrics, metric)
			var prettyVars []string
			for variableName, value := range variables {
				prettyVars = append(prettyVars, fmt.Sprintf("%s=%f", variableName, value))
			}
			slog.Debug("processed metric", slog.String("name", metricDef.Name), slog.String("expression", metricDef.Expression), slog.String("vars", strings.Join(prettyVars, ", ")))
		}
		metricFrames = append(metricFrames, metricFrame)
	}
	return
}

// topologicalSort sorts the metric definitions based on their dependencies.
func topologicalSort(metrics []MetricDefinition) ([]MetricDefinition, error) {
	// A map to quickly look up metric definitions by name.
	metricMap := make(map[string]MetricDefinition)
	for _, m := range metrics {
		metricMap[m.Name] = m
	}

	// The graph stores the dependencies, e.g., graph[A] = {B, C} means B and C depend on A.
	graph := make(map[string][]string)
	// The inDegree map stores the number of dependencies for each metric.
	inDegree := make(map[string]int)

	allMetricNames := mapset.NewSet[string]()
	for _, m := range metrics {
		allMetricNames.Add(m.Name)
	}

	for _, metric := range metrics {
		inDegree[metric.Name] = 0 // Initialize in-degree.
		graph[metric.Name] = []string{}
	}

	for _, metric := range metrics {
		vars := metric.Evaluable.Vars()
		for _, varName := range vars {
			// If a variable is another metric, it's a dependency.
			if allMetricNames.Contains(varName) {
				graph[varName] = append(graph[varName], metric.Name)
				inDegree[metric.Name]++
			}
		}
	}

	// The queue stores metrics with no dependencies.
	queue := make([]string, 0)
	for _, metric := range metrics {
		if inDegree[metric.Name] == 0 {
			queue = append(queue, metric.Name)
		}
	}

	var sorted []MetricDefinition
	for len(queue) > 0 {
		metricName := queue[0]
		queue = queue[1:]
		sorted = append(sorted, metricMap[metricName])

		for _, dependent := range graph[metricName] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != len(metrics) {
		return nil, fmt.Errorf("circular dependency detected in metric definitions")
	}

	return sorted, nil
}

// lock to protect metric variable map that holds the event group where a variable value will be retrieved
var metricVariablesLock = sync.RWMutex{}

// for each variable in a metric, set the best group from which to get its value
func loadMetricBestGroups(metric MetricDefinition, frame EventFrame) (err error) {
	// one thread at a time through this function, since it updates the metric variables map and this only needs to be done one time
	metricVariablesLock.Lock()
	defer metricVariablesLock.Unlock()
	// only load event groups one time for each metric
	loadGroups := false
	for variableName := range metric.Variables {
		if metric.Variables[variableName] == -1 { // group not yet set
			loadGroups = true
			break
		}
		if metric.Variables[variableName] == -2 { // tried previously and failed, don't try again
			err = fmt.Errorf("metric variable group assignment previously failed, skipping: %s", variableName)
			return
		}
	}
	if !loadGroups {
		return // nothing to do, already loaded
	}
	allVariableNames := mapset.NewSetFromMapKeys(metric.Variables)
	remainingVariableNames := allVariableNames.Clone()
	for {
		if remainingVariableNames.Cardinality() == 0 { // found matches for all
			break
		}
		// find group with the greatest number of event names that match the remaining variable names
		bestGroupIdx := -1
		bestMatches := 0
		var matchedNames mapset.Set[string]
		for groupIdx, group := range frame.EventGroups {
			groupEventNames := mapset.NewSetFromMapKeys(group.EventValues)
			intersection := remainingVariableNames.Intersect(groupEventNames)
			// if an event value is NaN, remove it from the intersection map with hopes we'll find a better match
			for _, name := range intersection.ToSlice() {
				if math.IsNaN(group.EventValues[name]) {
					intersection.Remove(name)
				}
			}
			if intersection.Cardinality() > bestMatches {
				bestGroupIdx = groupIdx
				bestMatches = intersection.Cardinality()
				matchedNames = intersection.Clone()
				if bestMatches == remainingVariableNames.Cardinality() {
					break
				}
			}
		}
		if bestGroupIdx == -1 { // no matches
			for _, variableName := range remainingVariableNames.ToSlice() {
				metric.Variables[variableName] = -2 // we tried and failed
			}
			err = fmt.Errorf("metric variables (%s) not found for metric: %s", strings.Join(remainingVariableNames.ToSlice(), ", "), metric.Name)
			break
		}
		// for each of the matched names, set the value and the group from which to retrieve the value next time
		for _, name := range matchedNames.ToSlice() {
			metric.Variables[name] = bestGroupIdx
		}
		remainingVariableNames = remainingVariableNames.Difference(matchedNames)
	}
	return
}

// get the variable values that will be used to evaluate the metric's expression
func getExpressionVariableValues(metric MetricDefinition, frame EventFrame, previousTimestamp float64, metadata Metadata, computedMetrics map[string]float64, allMetricNames mapset.Set[string]) (variables map[string]any, err error) {
	variables = make(map[string]any)
	eventVariables := make(map[string]int)

	for varName := range metric.Variables {
		if allMetricNames.Contains(varName) {
			value, ok := computedMetrics[varName]
			if !ok {
				return nil, fmt.Errorf("metric dependency not met: %s not found in computed metrics for metric %s", varName, metric.Name)
			}
			slog.Debug("using computed metric value", slog.String("metric", metric.Name), slog.String("variable", varName), slog.Float64("value", value))
			variables[varName] = value
		} else {
			eventVariables[varName] = metric.Variables[varName]
		}
	}

	if len(eventVariables) == 0 {
		return variables, nil
	}

	tempMetricDef := metric
	tempMetricDef.Variables = eventVariables

	if err = loadMetricBestGroups(tempMetricDef, frame); err != nil {
		return nil, fmt.Errorf("at least one of the variables couldn't be assigned to a group: %v", err)
	}

	metricVariablesLock.Lock()
	for varName, groupIndex := range tempMetricDef.Variables {
		metric.Variables[varName] = groupIndex
	}
	metricVariablesLock.Unlock()

	for variableName, groupIndex := range tempMetricDef.Variables {
		// value already exists
		if v := variables[variableName]; v != nil {
			slog.Debug("skip variable value lookup", slog.String("metric", metric.Name), slog.String("variable", variableName))
			continue
		}
		if groupIndex < 0 {
			return nil, fmt.Errorf("event group for variable %s not resolved for metric %s", variableName, metric.Name)
		}
		if groupIndex >= len(frame.EventGroups) {
			return nil, fmt.Errorf("variable %s assigned to group %d, but only %d groups available", variableName, groupIndex, len(frame.EventGroups))
		}
		if _, ok := frame.EventGroups[groupIndex].EventValues[variableName]; !ok {
			return nil, fmt.Errorf("metric variable's assigned group does not have the variable name: %s", variableName)
		}
		variables[variableName] = frame.EventGroups[groupIndex].EventValues[variableName] / (frame.Timestamp - previousTimestamp)
		if variableName == "cstate_core/c6-residency/" && flagGranularity != granularityCPU && metadata.ThreadsPerCore > 1 {
			variables[variableName] = variables[variableName].(float64) * float64(metadata.ThreadsPerCore)
		}
	}
	return variables, nil
}

// function to call evaluator so that we can catch panics that come from the evaluator
func evaluateExpression(metric MetricDefinition, variables map[string]any) (result any, err error) {
	defer func() {
		if errx := recover(); errx != nil {
			err = errx.(error)
		}
	}()
	if result, err = metric.Evaluable.Evaluate(variables); err != nil {
		err = fmt.Errorf("%v : %s : %s", err, metric.Name, metric.Expression)
	}
	return
}

// write json formatted events to raw file
func writeEventsToFile(path string, events [][]byte) (err error) {
	rawFile, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) // #nosec G304 G302
	if err != nil {
		slog.Error("failed to open raw file for writing", slog.String("error", err.Error()))
		return
	}
	defer rawFile.Close()
	for _, rawEvent := range events {
		rawEvent = append(rawEvent, []byte("\n")...)
		if _, err = rawFile.Write(rawEvent); err != nil {
			slog.Error("failed to write event to raw file", slog.String("error", err.Error()))
			return
		}
	}
	return
}
