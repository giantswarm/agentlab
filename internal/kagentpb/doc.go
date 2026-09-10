// Package kagentpb carries the kagent API v2 control-plane messages the lab's
// proofs send over gRPC-Web: kagent.api.v1alpha1's common.proto,
// agent_instances.proto (AgentInstanceService — create, get, list, delete an
// AgentInstance) and system.proto (SystemService — the controller's version
// and its view of Substrate: WorkerPools, ActorTemplates, actors, workers),
// generated with protoc-gen-go from the controller's own descriptor set or
// its proto tree. The turns themselves are lf.a2a.v1, taken from the A2A v1
// package the controller is built with (github.com/a2aproject/a2a-go/v2).
//
// Regenerate from a controller (reflection on) or from kagent's proto tree:
//
//	grpcurl -plaintext localhost:8083 describe > /dev/null  # reflection reachable
//	grpcurl -plaintext -protoset-out kagent.protoset localhost:8083 describe
//	protoc --descriptor_set_in=kagent.protoset \
//	  --go_out=. --go_opt=module=github.com/giantswarm/agentlab \
//	  '--go_opt=Mkagent/api/v1alpha1/common.proto=github.com/giantswarm/agentlab/internal/kagentpb;kagentpb' \
//	  '--go_opt=Mkagent/api/v1alpha1/agent_instances.proto=github.com/giantswarm/agentlab/internal/kagentpb;kagentpb' \
//	  '--go_opt=Mkagent/api/v1alpha1/system.proto=github.com/giantswarm/agentlab/internal/kagentpb;kagentpb' \
//	  kagent/api/v1alpha1/common.proto kagent/api/v1alpha1/agent_instances.proto kagent/api/v1alpha1/system.proto
//
// protoc-gen-go at the protobuf version go.mod pins:
// `go build -o protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go`.
package kagentpb
