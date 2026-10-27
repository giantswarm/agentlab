package lab

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// A person's own ChatGPT subscription as a codex model
// (platform.chatgptSubscription): the lab renders the chart's
// kagent.modelConfigs[] entry with credentialType chatgptOAuth and no
// apiKeySecret, and the chart derives the rest from it — muster's provider
// App chatgpt (OpenAI's device-code sign-in), kagent's installation provider
// chatgpt (chatgpt.com, /backend-api/codex/, Bearer) and the ModelConfig
// "ChatGPT (your subscription)". The lab adds what the chart refuses the
// entry without: muster's token encryption (the key oauth-encryption-key is
// in muster's OAuth Secret from the first boot) and component ranges at the
// first releases that serve the entry, where the chart's own admit older
// ones. Nothing is applied by hand; `agentlab chatgpt-test` is the proof.

// chatgptSubscriptionValues is the values template's
// platform.chatgptSubscription input: the model entry and the component
// ranges the lab renders, by component.
type chatgptSubscriptionValues struct {
	ModelConfig, Model string
	Ranges             map[string]string
}

// ChatGPTRange is the range the values template renders for a component
// under platform.chatgptSubscription, "" for none.
func (t *tmplData) ChatGPTRange(component string) string {
	if t.ChatGPTSubscription == nil {
		return ""
	}
	return t.ChatGPTSubscription.Ranges[component]
}

// chatgptComponentFloor is the first release of a component that serves a
// person's own ChatGPT login, and the range the lab renders when the chart's
// own admits an older one: the floor up to the next minor of the kagent line
// (its releases follow upstream's minor), up to the next major of muster.
type chatgptComponentFloor struct {
	component, floor, override string
}

// The first kagent and muster releases that serve the entry, and the ranges
// the lab renders from them.
const (
	kagentChatGPTFloor = "1.24.0"
	kagentChatGPTRange = ">=1.24.0 <1.25.0"
	musterChatGPTFloor = "5.51.0"
	musterChatGPTRange = ">=5.51.0 <6.0.0"
)

// chatgptComponentFloors are the components the chart checks against the
// entry (agent-platform.validateChatGPTPersonalLogin).
var chatgptComponentFloors = []chatgptComponentFloor{
	{component: componentKagent, floor: kagentChatGPTFloor, override: kagentChatGPTRange},
	{component: componentKagentCRDs, floor: kagentChatGPTFloor, override: kagentChatGPTRange},
	{component: componentMuster, floor: musterChatGPTFloor, override: musterChatGPTRange},
}

// chatgptSubscriptionValuesFor is the template's input with the switch on,
// nil otherwise. A chart that cannot be read gets the overrides: they are
// what a chart below the floors needs, and one at them takes them unchanged.
func chatgptSubscriptionValuesFor(cfg *config.Config) *chatgptSubscriptionValues {
	if !cfg.ChatGPTSubscriptionEnabled() {
		return nil
	}
	ranges, err := chatgptChartRangesOf(platformChartFor(cfg))
	if err != nil {
		note("the platform chart's component ranges could not be read (%v): the lab renders the ChatGPT login's floors", err)
	}
	return &chatgptSubscriptionValues{
		ModelConfig: config.ChatGPTSubscriptionModelConfig,
		Model:       cfg.ChatGPTSubscriptionModel(),
		Ranges:      chatgptRangeOverrides(ranges),
	}
}

// chatgptRangeOverrides are the ranges the lab renders over the chart's: a
// component whose chart range floors below the first release that serves
// the entry, or that the chart names no range of, gets its override.
func chatgptRangeOverrides(chart map[string]string) map[string]string {
	overrides := map[string]string{}
	for _, f := range chatgptComponentFloors {
		if !rangeFloorsAt(chart[f.component], f.floor) {
			overrides[f.component] = f.override
		}
	}
	return overrides
}

// rangeFloorRe finds the floor of a Flux semver range (`>=X.Y.Z…`).
var rangeFloorRe = regexp.MustCompile(`>=\s*v?([0-9]+\.[0-9]+\.[0-9]+)`)

// rangeFloorsAt reports whether a range's floor is at or above the version,
// the way the chart compares them (the floor's release part).
func rangeFloorsAt(versionRange, floor string) bool {
	m := rangeFloorRe.FindStringSubmatch(versionRange)
	if m == nil {
		return false
	}
	have, err := semver.NewVersion(m[1])
	if err != nil {
		return false
	}
	return !have.LessThan(semver.MustParse(floor))
}

// chatgptChartRangesOf reads the chart's components.<name>.versionRange of
// the floors' components, memoized per chart; a test points
// chatgptChartRangesProbe at a stub.
func chatgptChartRangesOf(chart platformChart) (map[string]string, error) {
	chatgptChartRangesMu.Lock()
	defer chatgptChartRangesMu.Unlock()
	key := chart.String()
	if r, ok := chatgptChartRangesCache[key]; ok {
		return r, nil
	}
	r, err := chatgptChartRangesProbe(chart)
	if err != nil {
		return nil, err
	}
	chatgptChartRangesCache[key] = r
	return r, nil
}

var (
	chatgptChartRangesMu    sync.Mutex
	chatgptChartRangesCache = map[string]map[string]string{}
	chatgptChartRangesProbe = func(chart platformChart) (map[string]string, error) {
		c, err := loadPlatformChart(chart)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, fmt.Errorf("%s has no chart", chart)
		}
		return chatgptChartRanges(c.Values), nil
	}
)

// chatgptChartRanges is the chart values' range of each floor's component.
func chatgptChartRanges(values map[string]any) map[string]string {
	components, _ := values[valuesComponents].(map[string]any)
	ranges := map[string]string{}
	for _, f := range chatgptComponentFloors {
		c, _ := components[f.component].(map[string]any)
		if r := stringValue(c, valuesVersionRange); r != "" {
			ranges[f.component] = r
		}
	}
	return ranges
}
