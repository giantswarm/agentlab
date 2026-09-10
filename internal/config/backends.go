package config

// The host model servers the lab runs against: one table, so adding a server
// is one literal here plus one in internal/lab (the probe, the inventory
// reader and the host-side fixes, which need no place in the configuration).
// The order is canonical — it is the order `agentlab configure` writes the
// backends in, the discovery's preference, and therefore which server the one
// model-manager treats as its default backend. New servers go LAST: the first
// entry is Primary() for every lab that already exists.

// hostServerSpec is what the configuration knows about a host model server.
type hostServerSpec struct {
	// kind is the name in agentlab.yaml and in model-manager's API.
	kind string
	// name is the server as messages name it.
	name string
	// port is the default API port autodetected endpoints assume.
	port int
}

var hostServers = []hostServerSpec{
	{ModelManagerBackendOllama, "Ollama", OllamaPort},
	{ModelManagerBackendLemonade, "Lemonade Server", LemonadePort},
	{ModelManagerBackendLMStudio, "LM Studio", LMStudioPort},
}

// The model servers the lab runs against on the host, which model-manager
// and agent pods reach through the kind docker network's gateway.
const (
	// ModelManagerBackendOllama is a host Ollama.
	ModelManagerBackendOllama = "ollama"
	// ModelManagerBackendLemonade is a host Lemonade Server
	// (lemonade-server.ai): FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on
	// GPU and CPU, behind one OpenAI-compatible API plus a management API.
	ModelManagerBackendLemonade = "lemonade"
	// ModelManagerBackendLMStudio is a host LM Studio (lmstudio.ai):
	// llama.cpp on GPU and CPU, MLX on Apple silicon, behind one
	// OpenAI-compatible API plus its own /api/v1. Needs 0.4.0 or newer, and
	// it cannot delete a model — that is `lms rm` on the host.
	ModelManagerBackendLMStudio = "lmstudio"
)

// The servers' default API ports, the ones the autodetected endpoints assume.
const (
	OllamaPort   = 11434
	LemonadePort = 13305
	LMStudioPort = 1234
)

// ModelManagerBackends lists the backends the lab accepts, in the canonical
// order `agentlab configure` writes them — which is also the preference for
// the one model-manager fronts.
var ModelManagerBackends = backendKinds()

func backendKinds() []string {
	kinds := make([]string, 0, len(hostServers))
	for _, s := range hostServers {
		kinds = append(kinds, s.kind)
	}
	return kinds
}

// hostServer is the table entry of a backend, nil for a kind the lab does not
// know (which Validate rejects when the configuration is loaded).
func hostServer(backend string) *hostServerSpec {
	for i := range hostServers {
		if hostServers[i].kind == backend {
			return &hostServers[i]
		}
	}
	return nil
}

// BackendPort is the default API port of a backend's server, 0 for a kind the
// lab does not know.
func BackendPort(backend string) int {
	if s := hostServer(backend); s != nil {
		return s.port
	}
	return 0
}

// BackendServerName is a backend's server as messages name it; an unknown
// kind is named by itself rather than mistaken for one of the known servers.
func BackendServerName(backend string) string {
	if s := hostServer(backend); s != nil {
		return s.name
	}
	return backend
}
