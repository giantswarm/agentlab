package lab

import (
	"maps"
	"reflect"
	"slices"
)

// platformValuesFiles reads the meta chart's values the way the lab installs
// them: the lab's render at valuesPath, then the platform.valuesFiles
// overlays in order, as repeated `helm -f` flags read them — maps merge,
// lists replace, the later file wins — with one exception. A component's
// `postRenderers` list carries the lab's own patches (postrenderers.go: the
// portal's HSTS header dropped at the edge, hostNetwork, the dex-localhost
// sidecar, the dev images), and a proof's overlay that brings a patch of its
// own must not take the lab's away: the overlay's entries are appended to the
// lab's, in overlay order, an entry already in the list skipped. An overlay
// that sets a component's postRenderers to null removes the lab's patches on
// purpose, and the overlays after it append to an empty list.
func platformValuesFiles(valuesPath string, overlays []string) (map[string]any, error) {
	values, err := helmValuesFiles(append([]string{valuesPath}, overlays...)...)
	if err != nil || len(overlays) == 0 {
		return values, err
	}
	lab, err := helmValuesFile(valuesPath)
	if err != nil {
		return nil, err
	}
	combined := map[string][]any{}
	for _, overlay := range overlays {
		set, err := helmValuesFile(overlay)
		if err != nil {
			return nil, err
		}
		for component, list := range statedPostRenderers(set) {
			got, seen := combined[component]
			if !seen {
				got = slices.Clone(statedPostRenderers(lab)[component])
			}
			if list == nil {
				combined[component] = []any{}
				continue
			}
			for _, entry := range list {
				if !slices.ContainsFunc(got, func(have any) bool { return reflect.DeepEqual(have, entry) }) {
					got = append(got, entry)
				}
			}
			combined[component] = got
		}
	}
	components, _ := values["components"].(map[string]any)
	for _, component := range slices.Sorted(maps.Keys(combined)) {
		block, ok := components[component].(map[string]any)
		if !ok {
			continue
		}
		if len(combined[component]) == 0 {
			delete(block, "postRenderers")
			continue
		}
		block["postRenderers"] = combined[component]
	}
	return values, nil
}

// statedPostRenderers is every `components.<name>.postRenderers` a values
// set states, keyed by component: the list, or nil where the set states the
// key as null (the deletion marker Helm's loader keeps) or as no list.
func statedPostRenderers(values map[string]any) map[string][]any {
	components, _ := values["components"].(map[string]any)
	found := map[string][]any{}
	for name, block := range components {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		v, ok := m["postRenderers"]
		if !ok {
			continue
		}
		list, _ := v.([]any)
		found[name] = list
	}
	return found
}
