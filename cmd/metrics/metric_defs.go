package metrics

// Copyright (C) 2021-2025 Intel Corporation
// SPDX-License-Identifier: BSD-3-Clause

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/casbin/govaluate"
	mapset "github.com/deckarep/golang-set/v2"
)

// configureAndFilterMetrics sets the evaluable expression for each metric, identifies the variables
// in the expression, and filters out metrics that are not collectable.
func configureAndFilterMetrics(metrics []MetricDefinition, uncollectableEvents []string, metadata Metadata) ([]MetricDefinition, error) {
	// First, parse all expressions and find all variables.
	for i := range metrics {
		metric := &metrics[i]
		expression, err := govaluate.NewEvaluableExpression(metric.Expression)
		if err != nil {
			return nil, fmt.Errorf("failed to create evaluable expression for metric %s: %w", metric.Name, err)
		}
		metric.Evaluable = expression
		metric.Variables = make(map[string]int)
		for _, v := range expression.Vars() {
			metric.Variables[v] = -1 // -1 means event group not yet set
		}
	}

	// Get all metric names for dependency checking.
	allMetricNames := mapset.NewSet[string]()
	for _, m := range metrics {
		allMetricNames.Add(m.Name)
	}

	uncollectableSet := mapset.NewSet[string](uncollectableEvents...)

	// Keep track of bad metrics that should be removed.
	badMetrics := mapset.NewSet[string]()

	// First pass: find metrics that directly depend on uncollectable events.
	for _, metric := range metrics {
		vars := metric.Evaluable.Vars()
		for _, varName := range vars {
			if !allMetricNames.Contains(varName) && uncollectableSet.Contains(varName) {
				badMetrics.Add(metric.Name)
				break
			}
		}
	}

	// Second pass: transitively find all metrics that depend on bad metrics.
	// This needs to run until no new bad metrics are found in an iteration.
	for {
		newlyFoundBadMetrics := mapset.NewSet[string]()
		for _, metric := range metrics {
			if badMetrics.Contains(metric.Name) {
				continue
			}
			vars := metric.Evaluable.Vars()
			for _, varName := range vars {
				if badMetrics.Contains(varName) {
					newlyFoundBadMetrics.Add(metric.Name)
					break
				}
			}
		}
		if newlyFoundBadMetrics.Cardinality() == 0 {
			break
		}
		badMetrics = badMetrics.Union(newlyFoundBadMetrics)
	}

	var configuredMetrics []MetricDefinition
	for _, metric := range metrics {
		if !badMetrics.Contains(metric.Name) {
			configuredMetrics = append(configuredMetrics, metric)
		}
	}

	if badMetrics.Cardinality() > 0 {
		slog.Info("some metrics were removed because they depend on uncollectable events", "metrics", strings.Join(badMetrics.ToSlice(), ", "))
	}

	return configuredMetrics, nil
}
