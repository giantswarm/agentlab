package lab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentplatform-kind/internal/config"
)

// healADKImages works around HACKS.md U8: kagent-controller composes the
// runtime image for `runtime: go` agents from its own release tag —
// <IMAGE_REGISTRY>/golang-adk:<IMAGE_TAG> (plain) or :<IMAGE_TAG>-full (when
// the agent mounts skills) — but Giant Swarm's retagging has been observed to
// lag for exactly this repo (0.9.12 published for controller/app/skills-init,
// not for golang-adk), leaving every created agent in ImagePullBackOff.
//
// The heal is self-converging: each run first pulls the real tag, so the
// moment upstream publishes it, any previously faked local tag is overwritten
// with the real bits and side-loaded. Only when the registry does not have
// the tag is the newest older release retagged in its place. Best-effort by
// design — a failed heal costs broken agent pods, not the platform install.
//
// Only the go runtime is healed: the lab's create flow deploys the `agent`
// chart, whose runtime defaults to (and stays) go.
func healADKImages(cfg *config.Config) {
	registry, err := outputQuiet("kubectl", "-n", "kagent", "get", "cm", "kagent-controller",
		"-o", "jsonpath={.data.IMAGE_REGISTRY}")
	if err != nil || registry == "" {
		note("cannot read kagent's IMAGE_REGISTRY; skipping the ADK image heal (HACKS.md U8)")
		return
	}
	tag, err := outputQuiet("kubectl", "-n", "kagent", "get", "cm", "kagent-controller",
		"-o", "jsonpath={.data.IMAGE_TAG}")
	if err != nil || tag == "" {
		note("cannot read kagent's IMAGE_TAG; skipping the ADK image heal (HACKS.md U8)")
		return
	}
	registry = strings.TrimSuffix(strings.TrimSpace(registry), "/")
	tag = strings.TrimSpace(tag)
	for _, variant := range []string{"", "-full"} {
		healADKImage(cfg, registry, tag, variant)
	}
}

func healADKImage(cfg *config.Config, registry, tag, variant string) {
	img := registry + "/golang-adk:" + tag + variant
	prevID := dockerImageID(img)
	// Probe with stderr swallowed: an unpublished tag is the expected trigger
	// for the fallback, not an error worth showing.
	if _, err := outputQuiet("docker", "pull", img); err != nil {
		fallback, ferr := latestADKFallback(registry, tag, variant)
		if ferr != nil {
			note("golang-adk:%s%s is not pullable and no fallback tag was found (%v) — created agents will ImagePullBackOff (HACKS.md U8)", tag, variant, ferr)
			return
		}
		if err := runQuiet("docker", "pull", fallback); err != nil {
			note("pulling %s failed (%v) — created agents will ImagePullBackOff (HACKS.md U8)", fallback, err)
			return
		}
		if err := runQuiet("docker", "tag", fallback, img); err != nil {
			note("retagging %s failed (%v)", fallback, err)
			return
		}
		note("golang-adk:%s%s is not published; standing in %s (HACKS.md U8)", tag, variant, fallback)
	}
	// Side-load when the node lacks the tag, or when the pull/retag above
	// changed what the local tag points to (e.g. upstream finally published
	// the real image over an earlier stand-in).
	id := dockerImageID(img)
	if id == "" {
		return
	}
	if id != prevID || !nodeHasImage(cfg.ControlPlaneNode(), img) {
		if err := runQuiet("kind", "load", "docker-image", img, "--name", cfg.ClusterName); err != nil {
			note("side-loading %s failed (%v)", img, err)
		}
	}
}

// dockerImageID returns the local docker image ID for a tag, or "" when the
// tag is absent from the host cache.
func dockerImageID(img string) string {
	id, err := outputQuiet("docker", "image", "inspect", "-f", "{{.Id}}", img)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

// latestADKFallback asks the registry for golang-adk's tags and returns the
// newest release below the wanted one, matching the variant exactly (plain
// tags for "", `-full` tags for "-full").
func latestADKFallback(registry, tag, variant string) (string, error) {
	host, repoBase, ok := strings.Cut(registry, "/")
	if !ok {
		return "", fmt.Errorf("unexpected registry %q", registry)
	}
	repo := repoBase + "/golang-adk"
	tags, err := registryTags(host, repo)
	if err != nil {
		return "", err
	}
	want, err := parseSemver(tag)
	if err != nil {
		return "", fmt.Errorf("kagent IMAGE_TAG %q: %w", tag, err)
	}
	var best [3]int
	found := false
	for _, t := range tags {
		base, hasVariant := strings.CutSuffix(t, "-full")
		if (variant == "-full") != hasVariant {
			continue
		}
		v, err := parseSemver(base)
		if err != nil {
			continue
		}
		if semverLess(v, want) && (!found || semverLess(best, v)) {
			best = v
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("no published %s tag below %s%s", repo, tag, variant)
	}
	return fmt.Sprintf("%s/golang-adk:%d.%d.%d%s", registry, best[0], best[1], best[2], variant), nil
}

var authParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// registryTags lists a repository's tags over the OCI distribution API,
// following the standard anonymous Bearer token challenge (how gsoci serves
// its public images).
func registryTags(host, repo string) ([]string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	url := "https://" + host + "/v2/" + repo + "/tags/list"
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		params := map[string]string{}
		for _, m := range authParamRe.FindAllStringSubmatch(challenge, -1) {
			params[m[1]] = m[2]
		}
		realm := params["realm"]
		if realm == "" {
			return nil, fmt.Errorf("registry %s: unauthorized without a Bearer challenge", host)
		}
		tokResp, err := client.Get(realm + "?service=" + params["service"] + "&scope=repository:" + repo + ":pull")
		if err != nil {
			return nil, err
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		err = json.NewDecoder(tokResp.Body).Decode(&tok)
		_ = tokResp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("registry token from %s: %w", realm, err)
		}
		if tok.Token == "" {
			tok.Token = tok.AccessToken
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s tags: %s", repo, resp.Status)
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list.Tags, nil
}

func parseSemver(s string) ([3]int, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("not a x.y.z version: %q", s)
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, fmt.Errorf("not a x.y.z version: %q", s)
		}
		v[i] = n
	}
	return v, nil
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
