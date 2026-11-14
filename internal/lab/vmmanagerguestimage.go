package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// The guest image download of vm-manager's pod, sized before the install.
//
// The pod's init container (vmManagerGuestImageInit) fetches the guest image
// — an OCI artifact of some 1.7 GiB — with vm-manager's own registry client,
// not containerd's: no side-load and no registry mirror reaches it, and the
// pod is not Ready, nor its release installed, until the last byte is in.
// Under helm-controller's default timeout (fluxDefaultTimeout) that download
// fails on a home uplink of a few MB/s, and every install remediation
// uninstalls the release — the state claim with it — while the fetch starts
// over from an empty directory: the release loops until the lab's own wait
// gives up. So the lab sizes the release's timeout from the artifact's size
// at a floor rate (vmManagerGuestImageFloorRate), extends its own wait by as
// much, and — when the pod is about to download — samples the link first and
// refuses one too slow for that bound, before anything is installed.
//
// A local build (platform.vmManager.imageDir) is not sized: the pod fetches
// it from the lab registry on the kind network.

const (
	// vmManagerGuestImageInit is the chart's init container that fetches the
	// guest image: `vm-manager image pull <reference> --image-dir=… [--plain-http]`.
	vmManagerGuestImageInit = "guest-image"
	// vmManagerGuestImageFloorRate is the slowest link the vm-manager
	// release's timeout is sized for, in bytes a second.
	vmManagerGuestImageFloorRate = 1 << 20
	// fluxDefaultTimeout is helm-controller's timeout for a HelmRelease
	// without spec.timeout: what the rest of the release gets on top of the
	// download.
	fluxDefaultTimeout = 5 * time.Minute
)

// The link sample: whichever of the two bounds comes first, read off the
// artifact's largest file and thrown away. Variables so tests do not wait.
var (
	guestImageSampleBytes  int64 = 8 << 20
	guestImageSampleWindow       = 8 * time.Second
)

// vmManagerGuestImageFetch is the download the vm-manager pod makes at its
// first start, as the render names it and the registry sizes it.
type vmManagerGuestImageFetch struct {
	// Reference the init container pulls, as rendered.
	Reference string
	// Size of the artifact's files, in bytes.
	Size int64
}

// releaseTimeout is the vm-manager HelmRelease's spec.timeout: Flux's
// default for everything else the release waits for, plus the download at
// the floor rate, rounded up to the minute. Depends on the artifact's size
// alone, so the values — and the Helm revision — stay as they are from one
// run to the next.
func (f *vmManagerGuestImageFetch) releaseTimeout() time.Duration {
	download := time.Duration(math.Ceil(float64(f.Size)/vmManagerGuestImageFloorRate)) * time.Second
	return time.Duration(math.Ceil((fluxDefaultTimeout + download).Minutes())) * time.Minute
}

// timeoutValue is releaseTimeout as the HelmRelease takes it ("33m"); empty
// without a fetch, which leaves the chart's (Flux's) default.
func (f *vmManagerGuestImageFetch) timeoutValue() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%dm", int(f.releaseTimeout().Minutes()))
}

// extraWait is how much longer than its usual bound the lab's install waits
// for the platform: the release's allowance beyond Flux's default.
func (f *vmManagerGuestImageFetch) extraWait() time.Duration {
	if f == nil {
		return 0
	}
	return f.releaseTimeout() - fluxDefaultTimeout
}

// renderedGuestImageRef reads the guest image reference off the vm-manager
// component's render: the argument of its init container's `image pull`
// that is not a flag, and whether it pulls over plain HTTP.
func renderedGuestImageRef(rendered string) (ref string, plainHTTP, ok bool) {
	for _, d := range renderedDeployments(rendered) {
		for _, c := range d.Spec.Template.Spec.InitContainers {
			if c.Name != vmManagerGuestImageInit {
				continue
			}
			pull := slices.Index(c.Args, "pull")
			if pull < 0 {
				return "", false, false
			}
			for _, arg := range c.Args[pull+1:] {
				switch {
				case arg == "--plain-http":
					plainHTTP = true
				case !strings.HasPrefix(arg, "-") && ref == "":
					ref = arg
				}
			}
			return ref, plainHTTP, ref != ""
		}
	}
	return "", false, false
}

// sizeVMManagerGuestImage resolves the guest image the vm-manager release's
// pod is about to fetch from the release's guest image registry, and refuses
// the install when the download cannot fit the release's timeout. Nil, with
// a note, when there is nothing to size: a local build in the lab registry,
// or no vm-manager render to read the reference off (the release keeps
// Flux's default).
func sizeVMManagerGuestImage(ctx context.Context, imageDir string, renders map[string]string) (*vmManagerGuestImageFetch, error) {
	if imageDir != "" {
		return nil, nil
	}
	ref, plainHTTP, ok := renderedGuestImageRef(renders[vmManagerMCPServer])
	if !ok {
		note("no vm-manager render names its guest image: its release keeps helm-controller's %s timeout", fluxDefaultTimeout)
		return nil, nil
	}
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("the vm-manager guest image reference %q: %w", ref, err)
	}
	repo.PlainHTTP = plainHTTP
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}

	var size int64
	var largest ocispec.Descriptor
	err = retryUnreachable("resolving the vm-manager guest image", func() (err error) {
		size, largest, err = guestImageSize(ctx, repo, ref)
		return err
	})
	if err != nil {
		return nil, guestImageUnavailableError(ref, err)
	}
	fetch := &vmManagerGuestImageFetch{Reference: ref, Size: size}
	if vmManagerHoldsGuestImage(ctx, ref) {
		note("vm-manager's pod holds its guest image %s already (%s): nothing to download", ref, formatBytes(size))
		return fetch, nil
	}
	rate, err := sampleBlobRate(ctx, repo, largest)
	if err != nil {
		return nil, guestImageUnavailableError(ref, err)
	}
	if err := judgeGuestImageLink(fetch, registryHost(ref), rate); err != nil {
		return nil, err
	}
	note("vm-manager's pod fetches its guest image at its first start: %s of %s, about %s at the %s measured; its release may take up to %s",
		formatBytes(size), ref, minutes(time.Duration(float64(size)/rate*float64(time.Second))), formatRate(rate), minutes(fetch.releaseTimeout()))
	return fetch, nil
}

// guestImageSize is the size of the artifact's files and its largest file,
// read off the manifest (one request; no file is fetched).
func guestImageSize(ctx context.Context, repo *remote.Repository, ref string) (int64, ocispec.Descriptor, error) {
	_, raw, err := oras.FetchBytes(ctx, repo, repo.Reference.ReferenceOrDefault(), oras.DefaultFetchBytesOptions)
	if err != nil {
		return 0, ocispec.Descriptor{}, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, ocispec.Descriptor{}, fmt.Errorf("%s: not an OCI manifest: %w", ref, err)
	}
	if len(manifest.Layers) == 0 {
		return 0, ocispec.Descriptor{}, fmt.Errorf("%s: the manifest names no files", ref)
	}
	var size int64
	largest := manifest.Layers[0]
	for _, layer := range manifest.Layers {
		size += layer.Size
		if layer.Size > largest.Size {
			largest = layer
		}
	}
	return size, largest, nil
}

// sampleBlobRate measures the link to the registry on one file of the
// artifact: bytes a second from the response's first byte until
// guestImageSampleBytes are in or guestImageSampleWindow has passed. A
// stalled body ends the sample when the window does, at what it read.
func sampleBlobRate(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*guestImageSampleWindow)
	defer cancel()
	body, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()
	start := time.Now()
	stop := time.AfterFunc(guestImageSampleWindow, cancel)
	defer stop.Stop()
	buf := make([]byte, 64<<10)
	var n int64
	for n < guestImageSampleBytes {
		m, err := body.Read(buf)
		n += int64(m)
		if errors.Is(err, io.EOF) || (err != nil && ctx.Err() != nil) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	return float64(n) / time.Since(start).Seconds(), nil
}

// judgeGuestImageLink refuses a link on which the download would outlast
// the release's allowance for it — the install would time out on the
// vm-manager release, uninstall it and fetch again from zero — with the
// fixes.
func judgeGuestImageLink(fetch *vmManagerGuestImageFetch, host string, rate float64) error {
	allowance := fetch.releaseTimeout() - fluxDefaultTimeout
	if rate > 0 && time.Duration(float64(fetch.Size)/rate*float64(time.Second)) <= allowance {
		return nil
	}
	estimate := "no byte arrived"
	if rate > 0 {
		estimate = "about " + minutes(time.Duration(float64(fetch.Size)/rate*float64(time.Second)))
	}
	return fmt.Errorf("vm-manager's pod fetches its guest image (%s of %s) at its first start, before its release is installed, and this link is too slow for it:\n"+
		"  %s measured from %s (%s), and its release waits %s for the download (sized for %s). The release would time out, uninstall and start the download over.\n"+
		"  Fix one of:\n"+
		"    - the link to %s: a VPN can route the registry over a stalling private path (`getent ahosts %s`; see docs/troubleshooting.md), then `agentlab platform`\n"+
		"    - boot a local guest image build from the lab registry instead: `agentlab configure --vm-manager-image-dir <a vm-manager checkout's images/build>`\n"+
		"    - turn vm-manager off: `agentlab configure --vm-manager=false`",
		formatBytes(fetch.Size), fetch.Reference, formatRate(rate), host, estimate, minutes(allowance), formatRate(vmManagerGuestImageFloorRate), host, host)
}

// guestImageUnavailableError is the refusal for a guest image the registry
// does not serve: the pod would fail its init container on it the same way,
// after the install's wait.
func guestImageUnavailableError(ref string, err error) error {
	why := "the registry did not answer"
	switch {
	case errors.Is(err, errdef.ErrNotFound):
		why = "the registry has no such artifact (a vm-manager release whose guest image was not published)"
	case !registryUnreachable(err):
		why = "the registry refused it"
	}
	return fmt.Errorf("vm-manager's pod fetches its guest image %s at its first start, and %s: %w\n"+
		"  Fix the host's network or DNS (`getent ahosts %s`) and run `agentlab platform`, boot a local build (`agentlab configure --vm-manager-image-dir <dir>`), or turn vm-manager off (`agentlab configure --vm-manager=false`)",
		ref, why, err, registryHost(ref))
}

// vmManagerHoldsGuestImage is true when the running vm-manager Deployment is
// available with an init container that pulled this very reference: its
// state claim holds the image, and the next pod start keeps it.
func vmManagerHoldsGuestImage(ctx context.Context, ref string) bool {
	k, err := labKube()
	if err != nil {
		return false
	}
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, vmManagerMCPServer, metav1.GetOptions{})
	if err != nil || d.Status.AvailableReplicas < 1 {
		return false
	}
	for _, c := range d.Spec.Template.Spec.InitContainers {
		if c.Name == vmManagerGuestImageInit && slices.Contains(c.Args, ref) {
			return true
		}
	}
	return false
}

// formatRate words a link rate in binary megabytes a second.
func formatRate(bytesPerSecond float64) string {
	return fmt.Sprintf("%.1f MiB/s", bytesPerSecond/(1<<20))
}

// minutes words a duration in whole minutes, rounded up.
func minutes(d time.Duration) string {
	return fmt.Sprintf("%d min", int(math.Ceil(d.Minutes())))
}
