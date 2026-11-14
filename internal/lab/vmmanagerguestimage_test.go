package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testGuestImageSize is the size of the vm-manager 0.24.10 guest image
// artifact: 1,677 MiB in its files.
const testGuestImageSize = 1758223515

// testGuestImageTimeout is the release timeout sized for it: 5m plus
// 1,677 s at 1 MiB/s, rounded up to the minute.
const testGuestImageTimeout = "33m"

// guestImageRender is a vm-manager component render whose init container
// pulls ref, with extra arguments after it.
func guestImageRender(ref string, extra ...string) string {
	args := fmt.Sprintf(`["image", "pull", %q, "--image-dir=/var/lib/vm-manager/images"`, ref)
	for _, a := range extra {
		args += fmt.Sprintf(", %q", a)
	}
	args += "]"
	return `---
apiVersion: v1
kind: Service
metadata:
  name: vm-manager
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vm-manager
spec:
  template:
    spec:
      initContainers:
        - name: guest-image
          image: gsoci.azurecr.io/giantswarm/vm-manager:0.24.10
          args: ` + args + `
      containers:
        - name: vm-manager
          image: gsoci.azurecr.io/giantswarm/vm-manager:0.24.10
`
}

// TestRenderedGuestImageRef: the reference is the argument of the init
// container's `image pull` that is not a flag; --plain-http is read too; a
// render without the init container names none.
func TestRenderedGuestImageRef(t *testing.T) {
	const ref = "gsoci.azurecr.io/giantswarm/vm-manager-guest-image:0.24.10"
	got, plain, ok := renderedGuestImageRef(guestImageRender(ref))
	if !ok || got != ref || plain {
		t.Fatalf("release render: got %q plainHTTP=%v ok=%v", got, plain, ok)
	}
	const local = "agentlab-registry:5000/vm-manager-guest-image:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, plain, ok = renderedGuestImageRef(guestImageRender(local, "--plain-http"))
	if !ok || got != local || !plain {
		t.Fatalf("lab registry render: got %q plainHTTP=%v ok=%v", got, plain, ok)
	}
	for name, rendered := range map[string]string{
		"no render":         "",
		"no init container": strings.Replace(guestImageRender(ref), "guest-image", "something-else", 1),
		"another command":   strings.Replace(guestImageRender(ref), `"pull"`, `"push"`, 1),
	} {
		if got, _, ok := renderedGuestImageRef(rendered); ok {
			t.Errorf("%s: read %q", name, got)
		}
	}
}

// TestVMManagerReleaseTimeout: Flux's 5m plus the download at 1 MiB/s,
// rounded up to the minute — 33m for the 0.24.10 artifact — and the lab's
// wait grows by what the release gets beyond 5m. No fetch, no timeout.
func TestVMManagerReleaseTimeout(t *testing.T) {
	f := &vmManagerGuestImageFetch{Size: testGuestImageSize}
	if got := f.timeoutValue(); got != testGuestImageTimeout {
		t.Errorf("timeout for %d bytes: %s, want %s", f.Size, got, testGuestImageTimeout)
	}
	if got := f.extraWait(); got != 28*time.Minute {
		t.Errorf("extra wait: %s, want 28m", got)
	}
	small := &vmManagerGuestImageFetch{Size: 10 << 20}
	if got := small.timeoutValue(); got != "6m" {
		t.Errorf("timeout for 10 MiB: %s, want 6m (rounded up)", got)
	}
	var none *vmManagerGuestImageFetch
	if none.timeoutValue() != "" || none.extraWait() != 0 {
		t.Errorf("no fetch: timeout %q, extra wait %s", none.timeoutValue(), none.extraWait())
	}
}

// TestJudgeGuestImageLink: a link the download fits on passes; a slower one
// and one that delivered nothing are refused with the measured rate, the
// estimate and the fixes.
func TestJudgeGuestImageLink(t *testing.T) {
	f := &vmManagerGuestImageFetch{Reference: "gsoci.azurecr.io/giantswarm/vm-manager-guest-image:0.24.10", Size: testGuestImageSize}
	if err := judgeGuestImageLink(f, "gsoci.azurecr.io", 2<<20); err != nil {
		t.Fatalf("2 MiB/s refused: %v", err)
	}
	err := judgeGuestImageLink(f, "gsoci.azurecr.io", 512<<10)
	if err == nil {
		t.Fatal("0.5 MiB/s passed")
	}
	for _, want := range []string{"0.5 MiB/s measured", "about 56 min", "waits 28 min", "--vm-manager-image-dir", "--vm-manager=false", "getent ahosts gsoci.azurecr.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	if err := judgeGuestImageLink(f, "gsoci.azurecr.io", 0); err == nil || !strings.Contains(err.Error(), "no byte arrived") {
		t.Errorf("a stalled link: %v", err)
	}
}

// fakeGuestImageRegistry serves one guest image manifest of two files whose
// declared sizes add up to size, and streams the larger file's body in
// chunks with pause between them, counting the blob requests.
func fakeGuestImageRegistry(t *testing.T, size int64, pause time.Duration) (host string, blobs *atomic.Int32) {
	t.Helper()
	small := ocispec.Descriptor{MediaType: "application/vnd.giantswarm.vm-manager.guest-image.file.v1", Digest: digest.FromString("policy"), Size: 1819}
	large := ocispec.Descriptor{MediaType: small.MediaType, Digest: digest.FromString("raw"), Size: size - small.Size}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.giantswarm.vm-manager.guest-image.v1",
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{small, large},
	})
	if err != nil {
		t.Fatal(err)
	}
	blobs = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/giantswarm/vm-manager-guest-image/manifests/0.24.10":
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(manifest).String())
			w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case r.URL.Path == "/v2/giantswarm/vm-manager-guest-image/blobs/"+large.Digest.String():
			blobs.Add(1)
			chunk := make([]byte, 4<<10)
			for i := 0; i < 64 && r.Context().Err() == nil; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(pause)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), blobs
}

// shortGuestImageSample bounds the link sample for a test.
func shortGuestImageSample(t *testing.T, bytes int64, window time.Duration) {
	t.Helper()
	prevBytes, prevWindow := guestImageSampleBytes, guestImageSampleWindow
	guestImageSampleBytes, guestImageSampleWindow = bytes, window
	t.Cleanup(func() { guestImageSampleBytes, guestImageSampleWindow = prevBytes, prevWindow })
}

// TestSizeVMManagerGuestImage: the release's guest image is sized off its
// manifest and the link sampled on its largest file; a fast link yields the
// sized timeout, a slow one is refused before the install, a missing
// artifact is refused as such, a pod that holds the image already is not
// sampled, and a local build is not sized at all.
func TestSizeVMManagerGuestImage(t *testing.T) {
	ctx := context.Background()
	shortGuestImageSample(t, 64<<10, 300*time.Millisecond)

	t.Run("fast link", func(t *testing.T) {
		newFakeLab(t)
		host, blobs := fakeGuestImageRegistry(t, testGuestImageSize, 0)
		ref := host + "/giantswarm/vm-manager-guest-image:0.24.10"
		fetch, err := sizeVMManagerGuestImage(ctx, "", map[string]string{vmManagerMCPServer: guestImageRender(ref, "--plain-http")})
		if err != nil {
			t.Fatal(err)
		}
		if fetch == nil || fetch.Size != testGuestImageSize || fetch.timeoutValue() != testGuestImageTimeout {
			t.Fatalf("sized %+v", fetch)
		}
		if blobs.Load() != 1 {
			t.Errorf("the link was sampled %d times, want once", blobs.Load())
		}
	})

	t.Run("slow link", func(t *testing.T) {
		newFakeLab(t)
		host, _ := fakeGuestImageRegistry(t, testGuestImageSize, 100*time.Millisecond)
		ref := host + "/giantswarm/vm-manager-guest-image:0.24.10"
		_, err := sizeVMManagerGuestImage(ctx, "", map[string]string{vmManagerMCPServer: guestImageRender(ref, "--plain-http")})
		if err == nil || !strings.Contains(err.Error(), "this link is too slow") {
			t.Fatalf("a ~40 KiB/s link: %v", err)
		}
	})

	t.Run("no such artifact", func(t *testing.T) {
		newFakeLab(t)
		host, _ := fakeGuestImageRegistry(t, testGuestImageSize, 0)
		ref := host + "/giantswarm/vm-manager-guest-image:0.20.1"
		_, err := sizeVMManagerGuestImage(ctx, "", map[string]string{vmManagerMCPServer: guestImageRender(ref, "--plain-http")})
		if err == nil || !strings.Contains(err.Error(), "the registry has no such artifact") {
			t.Fatalf("a missing artifact: %v", err)
		}
	})

	t.Run("the pod holds it", func(t *testing.T) {
		host, blobs := fakeGuestImageRegistry(t, testGuestImageSize, 100*time.Millisecond)
		ref := host + "/giantswarm/vm-manager-guest-image:0.24.10"
		lab := newFakeLab(t)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: vmManagerMCPServer, Namespace: platformNamespace},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: vmManagerGuestImageInit, Args: []string{"pull", ref, "--plain-http"}}},
			}}},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
		if _, err := lab.cs.AppsV1().Deployments(platformNamespace).Create(ctx, deploy, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		fetch, err := sizeVMManagerGuestImage(ctx, "", map[string]string{vmManagerMCPServer: guestImageRender(ref, "--plain-http")})
		if err != nil {
			t.Fatalf("a slow link refused although the pod holds the image: %v", err)
		}
		if fetch.timeoutValue() != testGuestImageTimeout || blobs.Load() != 0 {
			t.Errorf("timeout %q, %d samples: the release is sized, the link not sampled", fetch.timeoutValue(), blobs.Load())
		}
	})

	t.Run("local build", func(t *testing.T) {
		fetch, err := sizeVMManagerGuestImage(ctx, testImageDir, map[string]string{vmManagerMCPServer: guestImageRender("agentlab-registry:5000/vm-manager-guest-image:dev", "--plain-http")})
		if err != nil || fetch != nil {
			t.Fatalf("a local build was sized: %+v, %v", fetch, err)
		}
	})
}
