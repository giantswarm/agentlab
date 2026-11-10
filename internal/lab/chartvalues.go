package lab

import (
	"encoding/json"
	"sync"
)

// platformChartValues is what the meta chart about to be installed declares
// of its values: its values schema and its default values.
type platformChartValues struct {
	schema []byte
	values map[string]any
}

// carries reports whether the chart takes the key path (chartValuesCarry).
// The meta chart's schema root refuses unknown keys, so a block for a key it
// does not carry fails the install at render.
func (v platformChartValues) carries(path ...string) bool {
	return chartValuesCarry(v.schema, v.values, path...)
}

// platformChartValuesOf loads the chart once per chart and run: a registry
// chart is pulled once per digest, a directory read once. A failed load is
// not remembered, so the next caller asks again.
func platformChartValuesOf(chart platformChart) (platformChartValues, error) {
	platformChartValuesMu.Lock()
	defer platformChartValuesMu.Unlock()
	key := chart.String()
	if v, ok := platformChartValuesCache[key]; ok {
		return v, nil
	}
	v, err := platformChartValuesProbe(chart)
	if err != nil {
		return platformChartValues{}, err
	}
	platformChartValuesCache[key] = v
	return v, nil
}

var (
	platformChartValuesMu    sync.Mutex
	platformChartValuesCache = map[string]platformChartValues{}
	// platformChartValuesProbe loads the chart (loadPlatformChart); the tests
	// replace it, as no unit test reaches a registry.
	platformChartValuesProbe = func(chart platformChart) (platformChartValues, error) {
		c, err := loadPlatformChart(chart)
		if err != nil || c == nil {
			return platformChartValues{}, err
		}
		return platformChartValues{schema: c.Schema, values: c.Values}, nil
	}
)

// chartValuesCarry reports whether a chart's values schema names the key
// path (each key a property of the one before, from the root), or its
// default values carry it.
func chartValuesCarry(schema []byte, values map[string]any, path ...string) bool {
	if valuesCarry(values, path) {
		return true
	}
	if len(schema) == 0 {
		return false
	}
	raw := json.RawMessage(schema)
	for _, key := range path {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return false
		}
		next, ok := s.Properties[key]
		if !ok {
			return false
		}
		raw = next
	}
	return true
}

// valuesCarry reports whether values hold the key path, each key a mapping's.
func valuesCarry(values map[string]any, path []string) bool {
	for i, key := range path {
		v, ok := values[key]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		if values, ok = v.(map[string]any); !ok {
			return false
		}
	}
	return false
}
