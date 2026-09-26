package lab

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// The serving proof (`agentlab serving-test`): the llm-d path end to end, as
// a person drives it through the platform — the serving control plane the
// switch brought up, model-manager's kserve backend through muster as the
// person (its x_model-manager_* tools), the lab preset judged against the node and loaded, the
// LLMInferenceService the controller composes from the well-known template
// on the CPU runtime, the ModelConfig model-manager wires, a completion
// answered through the models Gateway with the person's token and refused
// without one, one agent turn on the wired ModelConfig, and the unload that
// leaves nothing behind.

// The KServe and Gateway API kinds the proof reads.
var (
	gvrLLMInferenceServices      = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices"}
	gvrLLMInferenceServiceConfig = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceserviceconfigs"}
	gvrGateways                  = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
)

// DefaultServingReadyTimeout bounds the serve: the storage-initializer's
// download of the weights (~1 GiB from the Hugging Face Hub) and the CPU
// runtime's start (the model loaded, the server answering /health).
const DefaultServingReadyTimeout = 20 * time.Minute

// The labels the llm-d controller puts on an LLMInferenceService's workload
// pods, and the label the agentgateway controller puts on a Gateway's data
// plane pods.
const (
	llmisvcPartOfLabel      = "app.kubernetes.io/part-of=llminferenceservice"
	llmisvcNameLabel        = "app.kubernetes.io/name"
	llmisvcMainContainer    = "main"
	gatewayDataPlaneLabel   = "gateway.networking.k8s.io/gateway-name"
	servingPresetLabel      = "agent-platform.giantswarm.io/preset-source"
	servingPresetConfigMap  = "agent-platform-serving-preset-"
	servingPresetSourceLab  = "values"
	servingTestAgent        = "agentlab-serving-test"
	kserveBackend           = config.ModelManagerBackendKServe
	completionsPath         = "/v1/chat/completions"
	chatRoleUser            = "user"
	servingPongPrompt       = "Reply with exactly the word pong and nothing else."
	llmisvcReadyCondition   = "Ready"
	gatewayProgrammedCond   = "Programmed"
	servingWaitLogInterval  = 30 * time.Second
	servingControlPlaneWait = 5 * time.Minute
)

// ServingTestOptions are the proof's knobs.
type ServingTestOptions struct {
	// Preset is the published preset to serve (default: the lab's).
	Preset string
	// SkipChat skips the agent turn on the wired ModelConfig.
	SkipChat bool
	// ReadyTimeout bounds the serve (download and start).
	ReadyTimeout time.Duration
}

// ServingTest runs the proof as the user.
func ServingTest(cfg *config.Config, email string, opts ServingTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.ServingEnabled() {
		return fmt.Errorf("platform.serving is off in %s (needs platform.agents too) — enable it (`agentlab configure --serving`) and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	preset := strings.TrimSpace(opts.Preset)
	if preset == "" {
		preset = servingPresetName
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultServingReadyTimeout
	}
	ctx := context.Background()
	k, err := labKube()
	if err != nil {
		return err
	}
	node := cfg.ControlPlaneNode()

	step("The serving control plane: the llm-d controller, the well-known template, the models Gateway, the lab preset")
	if err := waitDeploymentRolledOut(ctx, platformNamespace, llmisvcControllerDeployment, servingControlPlaneWait); err != nil {
		return fmt.Errorf("the llm-d controller is not up: %w (platform.serving is on; did `agentlab platform` install components.kserve-llmisvc-resources?)", err)
	}
	if _, err := getObject(ctx, gvrLLMInferenceServiceConfig, platformNamespace, llmisvcTemplateConfig); err != nil {
		return fmt.Errorf("the well-known template is not published: %w (components.kserve-runtime-configs)", err)
	}
	if err := waitCondition(ctx, gvrGateways, platformNamespace, modelsGatewayName, gatewayProgrammedCond, string(metav1.ConditionTrue), servingControlPlaneWait); err != nil {
		return fmt.Errorf("the models Gateway is not programmed: %w", err)
	}
	presetCM, err := k.clientset.CoreV1().ConfigMaps(platformNamespace).Get(ctx, servingPresetConfigMap+preset, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the preset %s is not published: %w (modelServing.presets in the lab's values)", preset, err)
	}
	note("llm-d controller %s rolled out; well-known %s present; Gateway %s programmed for %s; preset %s published (source %s)",
		llmisvcControllerDeployment, llmisvcTemplateConfig, modelsGatewayName, modelsGatewayHost(cfg), preset, presetCM.Labels[servingPresetLabel])

	onKServe := map[string]any{backendField: kserveBackend}
	if err := musterRefusesAnonymous(cfg); err != nil {
		return err
	}

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	note("got an id_token")
	session, err := openMusterSession(cfg, token, "serving-test")
	if err != nil {
		return err
	}
	api := modelManagerTools{session: session}

	step("The kserve backend through muster with the Dex token")
	var backend struct {
		Backend      string          `json:"backend"`
		Version      string          `json:"version"`
		Healthy      bool            `json:"healthy"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := api.getJSON("get_backend", onKServe, &backend); err != nil {
		return err
	}
	if backend.Backend != kserveBackend || !backend.Healthy {
		return fmt.Errorf("backend %q healthy=%v, wanted a healthy %s backend", backend.Backend, backend.Healthy, kserveBackend)
	}
	note("backend %s %s, healthy; capabilities: %s", backend.Backend, firstNonEmpty(backend.Version, "(reports no version)"), onCapabilities(backend.Capabilities))

	step("The published presets: the lab's %s among the shipped ones", preset)
	var presets struct {
		Presets []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			GPUs   int64  `json:"gpus"`
			Model  string `json:"model"`
		} `json:"presets"`
	}
	if err := api.getJSON("list_presets", onKServe, &presets); err != nil {
		return err
	}
	var shipped []string
	found := false
	presetModel := ""
	for _, p := range presets.Presets {
		if p.Name == preset {
			found = true
			presetModel = p.Model
			if p.GPUs != 0 {
				return fmt.Errorf("preset %s requests %d GPU(s); the lab preset requests none (the node has no GPU)", preset, p.GPUs)
			}
			note("%s: %s, source %s, gpus %d", p.Name, p.Model, p.Source, p.GPUs)
			continue
		}
		shipped = append(shipped, p.Name)
	}
	if !found {
		return fmt.Errorf("list_presets does not list %s (the connectivity chart publishes modelServing.presets as ConfigMaps model-manager reads)", preset)
	}
	slices.Sort(shipped)
	note("%d other presets published: %s", len(shipped), strings.Join(shipped, ", "))

	step("The fit: %s against the node's CPU capacity", preset)
	var fit struct {
		Fits          bool   `json:"fits"`
		Reason        string `json:"reason"`
		Node          string `json:"node"`
		BudgetSource  string `json:"budgetSource"`
		BudgetBytes   int64  `json:"budgetBytes"`
		RequiredBytes int64  `json:"requiredBytes"`
	}
	if err := api.getJSON("check_fit", map[string]any{"preset": preset, backendField: kserveBackend}, &fit); err != nil {
		return err
	}
	if !fit.Fits {
		return fmt.Errorf("%s does not fit: %s", preset, fit.Reason)
	}
	if fit.Node != node {
		return fmt.Errorf("the fit names node %q, wanted the kind node %s", fit.Node, node)
	}
	if fit.BudgetSource != "allocatable" {
		return fmt.Errorf("the fit's budget source is %q, wanted allocatable (a CPU preset is bounded by the node's memory)", fit.BudgetSource)
	}
	note("fits on %s: %s needed of %s allocatable", fit.Node, humanBytes(fit.RequiredBytes), humanBytes(fit.BudgetBytes))

	// A leftover of an interrupted run is unloaded first, so the load below
	// composes the object rather than finding it.
	if exists, err := objectExists(ctx, gvrLLMInferenceServices, servingNamespace, preset); err != nil {
		return err
	} else if exists {
		note("%s is left over from an earlier run — unloading it first", preset)
		if err := unloadServed(ctx, &api, preset); err != nil {
			return err
		}
	}

	step("Loading %s on %s (model-manager composes the LLMInferenceService as %s)", preset, kserveBackend, user.Email)
	started := time.Now()
	if _, err := api.call("load_model", map[string]any{"preset": preset, backendField: kserveBackend}); err != nil {
		return err
	}
	obj, err := getObject(ctx, gvrLLMInferenceServices, servingNamespace, preset)
	if err != nil {
		return fmt.Errorf("load_model succeeded but %s is not there: %w", describe(gvrLLMInferenceServices, servingNamespace, preset), err)
	}
	// A loading model says where it is: get_model carries the served entry's
	// phase and steps from the first read, not only once it is Ready — asked
	// by the preset and by the model it serves alike.
	for _, ref := range []string{preset, presetModel} {
		if ref == "" {
			continue
		}
		var loading struct {
			Running *struct {
				Phase string `json:"phase"`
				Steps []struct {
					Name  string `json:"name"`
					State string `json:"state"`
				} `json:"steps"`
			} `json:"running"`
		}
		if err := api.getJSON("get_model", map[string]any{modelField: ref, backendField: kserveBackend}, &loading); err != nil {
			return err
		}
		if loading.Running == nil || loading.Running.Phase == "" || len(loading.Running.Steps) == 0 {
			return fmt.Errorf("get_model %s right after the load reports no running phase and steps: a caller polling for readiness gets no signal", ref)
		}
		note("get_model %s while loading: phase %s, %d steps (%s %s first)", ref, loading.Running.Phase, len(loading.Running.Steps), loading.Running.Steps[0].Name, loading.Running.Steps[0].State)
	}
	image, _ := llmisvcMainImage(obj)
	uri, _, _ := unstructured.NestedString(obj.Object, "spec", "model", "uri")
	note("LLMInferenceService %s/%s: model %s, template image %s", servingNamespace, preset, uri, firstNonEmpty(image, "(the well-known template's)"))
	if image != servingRuntimeImage {
		return fmt.Errorf("the composed object names the runtime image %q, wanted the lab preset's %s", image, servingRuntimeImage)
	}

	step("Waiting for %s to serve (the weights download, the CPU runtime starts; up to %s)", preset, opts.ReadyTimeout)
	pod, err := waitLLMInferenceServiceReady(ctx, k, preset, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if pod.Spec.NodeName != node {
		return fmt.Errorf("the workload pod runs on %q, wanted the kind node %s", pod.Spec.NodeName, node)
	}
	main := containerNamed(pod, llmisvcMainContainer)
	if main == nil {
		return fmt.Errorf("the workload pod %s has no container %s", pod.Name, llmisvcMainContainer)
	}
	if main.Image != servingRuntimeImage {
		return fmt.Errorf("the workload pod runs %s, wanted %s", main.Image, servingRuntimeImage)
	}
	for _, list := range []corev1.ResourceList{main.Resources.Requests, main.Resources.Limits} {
		for name := range list {
			if strings.Contains(string(name), "gpu") {
				return fmt.Errorf("the workload pod requests %s; a CPU preset requests no accelerator", name)
			}
		}
	}
	note("Ready after %s: pod %s on %s, %s, no accelerator requested (%s)", time.Since(started).Round(time.Second), pod.Name, pod.Spec.NodeName, main.Image, imageOrigin(pod, llmisvcMainContainer))

	step("The served model as model-manager reports it (list_loaded_models)")
	var loaded struct {
		Models []servedModel `json:"loaded"`
	}
	if err := api.getJSON("list_loaded_models", onKServe, &loaded); err != nil {
		return err
	}
	var endpoint string
	for _, m := range loaded.Models {
		if m.Preset != preset && m.Name != preset {
			continue
		}
		if m.Kind != "LLMInferenceService" {
			return fmt.Errorf("list_loaded_models lists %s as a %s, wanted an LLMInferenceService (the llm-d path)", m.Name, m.Kind)
		}
		if !strings.HasPrefix(m.Endpoint, modelsGatewayURL(cfg)+"/") {
			return fmt.Errorf("list_loaded_models reports the endpoint %s, wanted the model's route on the models Gateway (%s/%s/%s)", m.Endpoint, modelsGatewayURL(cfg), servingNamespace, preset)
		}
		endpoint = m.Endpoint
		note("%s: %s, %s, routed at %s (node %s)", m.Name, m.Kind, m.Status, m.Endpoint, m.Node)
	}
	if endpoint == "" {
		return fmt.Errorf("list_loaded_models does not list %s", preset)
	}

	step("The ModelConfig model-manager wired into kagent")
	mcName := ""
	if !waitFor(30, 2*time.Second, func() bool {
		var m struct {
			ModelConfig struct {
				Name string `json:"name"`
			} `json:"modelConfig"`
		}
		if err := api.getJSON("get_model", map[string]any{modelField: servedModelName(preset, loaded.Models), backendField: kserveBackend}, &m); err != nil {
			return false
		}
		mcName = m.ModelConfig.Name
		return mcName != ""
	}) {
		return fmt.Errorf("get_model %s never reported a wired ModelConfig", preset)
	}
	if err := waitModelConfigAccepted(mcName); err != nil {
		return err
	}
	mc, err := readKagentObject(modelConfigResource, mcName)
	if err != nil {
		return err
	}
	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	if baseURL != endpoint+"/v1" {
		return fmt.Errorf("ModelConfig %s points at %q, wanted the served model's route %s/v1", mcName, baseURL, endpoint)
	}
	mcSpec := modelConfigSummary(mc)
	if !strings.Contains(mcSpec, " backend="+kserveBackend+" ") {
		return fmt.Errorf("ModelConfig %s does not carry the model-manager.giantswarm.io/backend=%s label: %s", mcName, kserveBackend, mcSpec)
	}
	note("ModelConfig %s: %s", mcName, strings.TrimSpace(mcSpec))

	step("A completion through the models Gateway (%s): no token refused, the person's token answered", modelsGatewayHost(cfg))
	pong, err := completionThroughModelsGateway(ctx, cfg, k, endpoint, token)
	if err != nil {
		return err
	}
	note("the model answered: %q", excerpt(pong, 120))

	// The agent turn is the lab's documented negative: the wired ModelConfig
	// sends the agent to the models Gateway, whose certificate is the lab
	// CA's, and the agent runtime (the Go ADK Harness in its Substrate
	// sandbox) trusts the public roots of its image and nothing else — the
	// platform has no knob to hand it another CA, and on an installation the
	// Gateway's certificate is a public one. The turn is driven all the same:
	// it must fail on exactly that verification and on nothing else, which
	// proves the runtime dials the route the ModelConfig names.
	agentTurn := "skipped (--skip-chat)"
	if !opts.SkipChat {
		step("Agent turn on %s: an agent created through agent-manager as %s, one A2A turn through the edge (runtime -> the models Gateway -> the CPU runtime)", mcName, user.Email)
		reply, err := servingAgentTurn(cfg, session, token, mcName)
		switch {
		case err == nil:
			note("agent replied: %q", excerpt(reply, 120))
			agentTurn = fmt.Sprintf("answered %q", excerpt(reply, 60))
		case isUntrustedGatewayCert(err):
			note("the runtime dialled %s and refused the lab CA's certificate — the lab's documented negative (the Harness trusts its image's public roots; an installation's Gateway certificate is a public one): %s", endpoint, excerptEnds(err.Error(), 160))
			agentTurn = "the runtime dialled the route and refused the lab CA's certificate (the documented negative)"
		default:
			return err
		}
	}

	step("Unloading %s", preset)
	if err := unloadServed(ctx, &api, preset); err != nil {
		return err
	}
	if err := waitModelConfigGone(mcName); err != nil {
		return err
	}
	note("LLMInferenceService gone, ModelConfig %s gone", mcName)

	fmt.Println()
	fmt.Printf("PASS: the serving control plane — llm-d controller, well-known %s, models Gateway %s, preset %s published\n", llmisvcTemplateConfig, modelsGatewayHost(cfg), preset)
	fmt.Printf("PASS: no token -> 401 at muster; Dex token -> model-manager's kserve backend as x_%s_* through muster\n", modelManagerMCPServer)
	fmt.Printf("PASS: fit on %s (allocatable budget) -> load -> LLMInferenceService Ready on %s, no accelerator -> ModelConfig %s wired at the model's route\n", node, servingRuntimeImage, mcName)
	fmt.Printf("PASS: %s%s: 401 without a token, a completion with %s's token\n", endpoint, completionsPath, user.Email)
	fmt.Printf("PASS: the agent turn on the wired ModelConfig: %s\n", agentTurn)
	fmt.Println("PASS: unload -> LLMInferenceService and ModelConfig gone")
	return nil
}

// isUntrustedGatewayCert reports whether an agent turn failed on the one
// thing the lab cannot give the runtime: trust in the lab CA that signs the
// models Gateway's certificate.
func isUntrustedGatewayCert(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "x509: certificate signed by unknown authority") ||
		strings.Contains(msg, "tls: failed to verify certificate")
}

// servedModel is one entry of list_loaded_models on the kserve backend.
type servedModel struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Preset   string `json:"preset"`
	Node     string `json:"node"`
}

// servedModelName is the reference get_model takes for the served
// preset: the model id model-manager lists it under, else the preset name.
func servedModelName(preset string, models []servedModel) string {
	for _, m := range models {
		if m.Preset == preset {
			return m.Name
		}
	}
	return preset
}

// onCapabilities words a capabilities map as the names that are on.
func onCapabilities(caps map[string]bool) string {
	on := make([]string, 0, len(caps))
	for name, ok := range caps {
		if ok {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	return strings.Join(on, ", ")
}

// llmisvcMainImage is the image an LLMInferenceService's template names for
// its main container, "" when the well-known template's stands.
func llmisvcMainImage(obj *unstructured.Unstructured) (string, bool) {
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "containers")
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if ok && cm["name"] == llmisvcMainContainer {
			image, _ := cm["image"].(string)
			return image, true
		}
	}
	return "", false
}

// containerNamed is the pod's container of that name, nil when absent.
func containerNamed(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// imageOrigin says where the container's image came from, as the node
// reports it: the image ID the kubelet resolved.
func imageOrigin(pod *corev1.Pod, container string) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == container && s.ImageID != "" {
			return "image ID " + excerptEnds(s.ImageID, 80)
		}
	}
	return "image ID unknown"
}

// waitLLMInferenceServiceReady waits until the object reads Ready and returns
// its workload pod, noting the pod's state along the way (the
// storage-initializer downloading, the runtime starting).
func waitLLMInferenceServiceReady(ctx context.Context, k *kubeClients, name string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	lastNote := time.Time{}
	lastState := ""
	selector := llmisvcPartOfLabel + "," + llmisvcNameLabel + "=" + name
	for time.Now().Before(deadline) {
		obj, err := getObject(ctx, gvrLLMInferenceServices, servingNamespace, name)
		if err != nil {
			return nil, err
		}
		pods, _ := k.clientset.CoreV1().Pods(servingNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		var pod *corev1.Pod
		if pods != nil && len(pods.Items) > 0 {
			pod = &pods.Items[0]
		}
		state := podStateSummary(pod)
		if pod != nil {
			for _, s := range pod.Status.InitContainerStatuses {
				if s.State.Running != nil {
					state += " (init " + s.Name + " running)"
				}
			}
		}
		ready := conditionStatus(obj, llmisvcReadyCondition) == string(metav1.ConditionTrue)
		if ready && pod != nil && pod.Status.Phase == corev1.PodRunning {
			return pod, nil
		}
		if state != lastState || time.Since(lastNote) > servingWaitLogInterval {
			note("%s: %s=%s, pod %s", name, llmisvcReadyCondition, firstNonEmpty(conditionStatus(obj, llmisvcReadyCondition), "unset"), state)
			lastNote, lastState = time.Now(), state
		}
		time.Sleep(5 * time.Second)
	}
	obj, _ := getObject(ctx, gvrLLMInferenceServices, servingNamespace, name)
	msg := ""
	if obj != nil {
		msg = conditionMessage(obj, llmisvcReadyCondition)
	}
	return nil, fmt.Errorf("%s did not serve within %s (%s: %s); `kubectl -n %s describe llminferenceservice %s` and the workload pod's logs say why", name, timeout, llmisvcReadyCondition, firstNonEmpty(msg, "no condition message"), servingNamespace, name)
}

// unloadServed unloads the preset through model-manager and waits until its
// LLMInferenceService is gone.
func unloadServed(ctx context.Context, api *modelManagerTools, preset string) error {
	if _, err := api.call("unload_model", map[string]any{modelField: preset, backendField: kserveBackend}); err != nil && refusalCode(err) != "not_found" {
		return err
	}
	gone := waitFor(60, 2*time.Second, func() bool {
		exists, err := objectExists(ctx, gvrLLMInferenceServices, servingNamespace, preset)
		return err == nil && !exists
	})
	if !gone {
		return fmt.Errorf("%s still exists after the unload", describe(gvrLLMInferenceServices, servingNamespace, preset))
	}
	return nil
}

// completionThroughModelsGateway drives the served model's route on the
// models Gateway the way a person would — the Gateway's data plane reached
// through a port-forward (kind publishes no LoadBalancer), TLS on the lab CA
// for the Gateway's hostname: a request without a token is refused with 401
// at the Gateway, one with the person's Dex token reaches vLLM and returns a
// completion, whose text is returned.
func completionThroughModelsGateway(ctx context.Context, cfg *config.Config, k *kubeClients, endpoint, token string) (string, error) {
	pods, err := k.clientset.CoreV1().Pods(platformNamespace).List(ctx, metav1.ListOptions{LabelSelector: gatewayDataPlaneLabel + "=" + modelsGatewayName})
	if err != nil {
		return "", fmt.Errorf("listing the models Gateway's data plane pods: %w", err)
	}
	var dataPlane *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			dataPlane = &pods.Items[i]
			break
		}
	}
	if dataPlane == nil {
		return "", fmt.Errorf("no running data plane pod of Gateway %s in %s (label %s)", modelsGatewayName, platformNamespace, gatewayDataPlaneLabel)
	}
	port, stop, err := portForwardPod(ctx, platformNamespace, dataPlane.Name, 443)
	if err != nil {
		return "", err
	}
	defer stop()
	client, err := modelsGatewayClient(cfg, port)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      servingPresetModel,
		"messages":   []map[string]string{{"role": chatRoleUser, "content": servingPongPrompt}},
		"max_tokens": 16,
	})
	do := func(bearer string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+completionsPath, strings.NewReader(string(payload)))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		// The status is the verdict here (a 401 is the expected answer to the
		// first request), so the body is read whatever it is.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
		return resp.StatusCode, body, err
	}
	status, body, err := do("")
	if err != nil {
		return "", fmt.Errorf("POST %s%s without a token: %w", endpoint, completionsPath, err)
	}
	if status != http.StatusUnauthorized {
		return "", fmt.Errorf("POST %s%s without a token answered %d, wanted 401 (the Gateway's JWT policy): %.200s", endpoint, completionsPath, status, body)
	}
	note("401 without a token at the Gateway")
	status, body, err = do(token)
	if err != nil {
		return "", fmt.Errorf("POST %s%s with the token: %w", endpoint, completionsPath, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("POST %s%s with the token answered %d: %.300s", endpoint, completionsPath, status, body)
	}
	var completion struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return "", fmt.Errorf("parsing the completion %.200s: %w", body, err)
	}
	if len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("the completion carries no text: %.300s", body)
	}
	note("200 with the token: model %s answered through %s", completion.Model, dataPlane.Name)
	return completion.Choices[0].Message.Content, nil
}

// modelsGatewayClient is an HTTP client for the models Gateway's hostname
// whose connections land on the port-forwarded data plane: the hostname is
// dialed on the loopback port, TLS verifies the lab's wildcard certificate
// for that name.
func modelsGatewayClient(cfg *config.Config, localPort int) (*http.Client, error) {
	pool, err := labCertPool()
	if err != nil {
		return nil, err
	}
	host := modelsGatewayHost(cfg)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if h, _, err := net.SplitHostPort(addr); err == nil && h == host {
				addr = net.JoinHostPort("127.0.0.1", fmt.Sprint(localPort))
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Timeout: 3 * time.Minute, Transport: transport}, nil
}

// servingAgentTurn is models-test's agent turn on the wired ModelConfig with
// the proof's own throwaway agent name.
func servingAgentTurn(cfg *config.Config, session *musterSession, token, modelConfig string) (string, error) {
	return agentTurnNamed(cfg, session, token, modelConfig, servingPongPrompt, servingTestAgent)
}
