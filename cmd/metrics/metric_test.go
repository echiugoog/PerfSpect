package metrics

import (
	"reflect"
	"testing"

	"github.com/casbin/govaluate"
)

func TestTopologicalSort(t *testing.T) {
	// Helper to create a dummy EvaluableExpression
	newEval := func(expr string) *govaluate.EvaluableExpression {
		eval, err := govaluate.NewEvaluableExpression(expr)
		if err != nil {
			t.Fatalf("Failed to create evaluable expression: %v", err)
		}
		return eval
	}

	// Test cases
	testCases := []struct {
		name          string
		metrics       []MetricDefinition
		expectedOrder []string
		expectError   bool
	}{
		{
			name: "Simple dependency chain",
			metrics: []MetricDefinition{
				{Name: "C", Expression: "B * 2", Evaluable: newEval("B * 2")},
				{Name: "A", Expression: "1", Evaluable: newEval("1")},
				{Name: "B", Expression: "A + 1", Evaluable: newEval("A + 1")},
			},
			expectedOrder: []string{"A", "B", "C"},
			expectError:   false,
		},
		{
			name: "Circular dependency",
			metrics: []MetricDefinition{
				{Name: "A", Expression: "B", Evaluable: newEval("B")},
				{Name: "B", Expression: "A", Evaluable: newEval("A")},
			},
			expectError: true,
		},
		{
			name: "No dependencies",
			metrics: []MetricDefinition{
				{Name: "A", Expression: "1", Evaluable: newEval("1")},
				{Name: "B", Expression: "2", Evaluable: newEval("2")},
				{Name: "C", Expression: "3", Evaluable: newEval("3")},
			},
			expectedOrder: []string{"A", "B", "C"}, // Order can vary, but any is valid.
			expectError:   false,
		},
		{
			name: "Complex dependencies",
			metrics: []MetricDefinition{
				{Name: "D", Expression: "B + C", Evaluable: newEval("B+C")},
				{Name: "C", Expression: "A", Evaluable: newEval("A")},
				{Name: "B", Expression: "A", Evaluable: newEval("A")},
				{Name: "A", Expression: "1", Evaluable: newEval("1")},
			},
			expectedOrder: []string{"A", "C", "B", "D"}, // or A, B, C, D
			expectError:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sorted, err := topologicalSort(tc.metrics)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected an error, but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			sortedNames := make([]string, len(sorted))
			for i, m := range sorted {
				sortedNames[i] = m.Name
			}

			// For cases with no dependencies or multiple valid orders, we check if the set of names is the same.
			if tc.name == "No dependencies" {
				if !reflect.DeepEqual(sortedNames, tc.expectedOrder) {
					// try another valid order
					altOrder := []string{"C", "B", "A"}
					if !reflect.DeepEqual(sortedNames, altOrder) {
						t.Errorf("Expected %v or %v, got %v", tc.expectedOrder, altOrder, sortedNames)
					}
				}
			} else if tc.name == "Complex dependencies" {
				altOrder := []string{"A", "B", "C", "D"}
				if !reflect.DeepEqual(sortedNames, tc.expectedOrder) && !reflect.DeepEqual(sortedNames, altOrder) {
					t.Errorf("Expected %v or %v, got %v", tc.expectedOrder, altOrder, sortedNames)
				}

			} else {
				if !reflect.DeepEqual(sortedNames, tc.expectedOrder) {
					t.Errorf("Expected order %v, but got %v", tc.expectedOrder, sortedNames)
				}
			}
		})
	}
}
