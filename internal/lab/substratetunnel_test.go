package lab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestJudgeTunnelClose(t *testing.T) {
	timeout := fmt.Errorf("Post: %w (Client.Timeout exceeded while awaiting headers)", context.DeadlineExceeded)
	for _, tt := range []struct {
		name   string
		answer routerAnswer
		want   string // "" passes; else a part of the error
	}{
		{"502 at once", routerAnswer{proto: http2Proto, status: http.StatusBadGateway, took: 40 * time.Millisecond}, ""},
		{"gRPC Unavailable at once", routerAnswer{proto: http2Proto, status: http.StatusOK, grpcStatus: "14", took: time.Second}, ""},
		{"the client's timeout", routerAnswer{err: timeout, took: 30 * time.Second}, "waited out its client's timeout"},
		{"502 late", routerAnswer{proto: http2Proto, status: http.StatusBadGateway, took: 29 * time.Second}, "failed loudly but late"},
		{"downgraded", routerAnswer{proto: http2Proto, status: http.StatusBadRequest, took: time.Second}, "must not be downgraded"},
		{"gRPC OK", routerAnswer{proto: http2Proto, status: http.StatusOK, grpcStatus: "0", took: time.Second}, "must not be downgraded"},
		{"another error", routerAnswer{err: errors.New("connection refused"), took: time.Millisecond}, "want HTTP 502 or gRPC Unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := judgeTunnelClose(tt.answer, SubstrateTunnelPromptly, "1.11.4")
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("judgeTunnelClose(%s) = %v, want a pass", tt.answer, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("judgeTunnelClose(%s) = %v, want an error carrying %q", tt.answer, err, tt.want)
			}
		})
	}
}

// The actor's script serves the health path and closes on the HTTP/2
// preface without an answer: both branches must be in it, on port 80.
func TestSubstrateTunnelActorScript(t *testing.T) {
	for _, want := range []string{
		`[ "$method" = PRI ] && exit 0`,
		"[ \"$path\" = " + substrateTunnelHealthPath + " ]",
		"Content-Length: 3",
		substrateTunnelHealthBody + `\n'`,
		"exec nc -lk -p 80 -e sh /tmp/serve",
	} {
		if !strings.Contains(substrateTunnelActorScript, want) {
			t.Errorf("the actor script lacks %q", want)
		}
	}
	if len(substrateTunnelHealthBody)+1 != 3 {
		t.Errorf("Content-Length 3 does not fit the body %q and its newline", substrateTunnelHealthBody)
	}
}

func TestSubstrateTunnelTemplateFrom(t *testing.T) {
	tmpl := substrateTunnelTemplateFrom(workspacesActorRuntime{workerPool: "pool", snapshotLocation: "loc"})
	if got := tmpl.GetWorkerSelector().GetMatchLabels()[workspacesKagentWorkerPoolLabel]; got != "pool" {
		t.Errorf("worker selector = %q, want pool", got)
	}
	if got := tmpl.GetSnapshotConfig().GetStorageLocation(); got != "loc" {
		t.Errorf("snapshot location = %q, want loc", got)
	}
	c := tmpl.GetContainers()
	if len(c) != 1 || c[0].GetImage() != workspacesActorImage || !strings.Contains(c[0].GetImage(), "@sha256:") {
		t.Fatalf("containers = %v, want one on the digest-pinned alpine", c)
	}
}

func TestServiceTargetPort(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Port: 80, TargetPort: intstr.FromInt32(8080)},
		{Port: 4040, TargetPort: intstr.FromString("named-port")},
		{Port: 9000},
		{Port: 15020, TargetPort: intstr.FromString("missing")},
	}}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{{Name: "named-port", ContainerPort: 4041}}}}}}
	for _, tt := range []struct {
		port    int32
		want    int
		wantErr bool
	}{{80, 8080, false}, {4040, 4041, false}, {9000, 9000, false}, {15020, 0, true}, {443, 0, true}} {
		got, err := serviceTargetPort(svc, pod, tt.port)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("serviceTargetPort(%d) = %d, %v; want %d, error %v", tt.port, got, err, tt.want, tt.wantErr)
		}
	}
}
