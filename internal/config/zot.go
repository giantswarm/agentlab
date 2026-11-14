package config

import (
	"fmt"
	"regexp"

	"github.com/Masterminds/semver/v3"
)

// The lab runs zot twice — the lab registry of each lab and the registry
// mirror every lab on the machine shares (docs/registries.md) — and pins
// neither: both follow zot's releases on gsoci, resolved once per run.

// ZotRepository is zot's image repository, gsoci's mirror of the project's
// release images, built with the extensions the mirror uses (sync).
const ZotRepository = "gsoci.azurecr.io/giantswarm/zot"

// ZotMajor is the zot major the lab follows. A new major may change the
// configuration format both containers are written in, so following one is
// a deliberate change, not a release picked up on the next run.
const ZotMajor = 2

// ZotFloor is the oldest zot release with what the lab's zot configurations
// use: the mirror's sparse multi-arch on-demand sync (2.1.22: only the
// platforms a node asks for, not every platform of an index) and its
// streaming (2.1.23: a blob served while it downloads). A minimum, not a
// pin; it moves only when those configurations start using something newer.
var ZotFloor = semver.MustParse("2.1.23")

// zotReleaseTagRe is the shape of a zot release tag: v<major>.<minor>.<patch>
// and nothing else — gsoci also carries v2.0.2-rc1 and v2.1.11-test, and
// semver's own lenient parser would read a bare v2.1 as 2.1.0.
var zotReleaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ParseZotRelease reads a zot image tag as a release of the major the lab
// follows (ZotMajor): false for a prerelease, a test build, any other shape
// and any other major.
func ParseZotRelease(tag string) (*semver.Version, bool) {
	if !zotReleaseTagRe.MatchString(tag) {
		return nil, false
	}
	v, err := semver.NewVersion(tag)
	if err != nil || v.Major() != ZotMajor {
		return nil, false
	}
	return v, true
}

// PickZotRelease is the zot the lab follows among a repository's tags: the
// highest release of ZotMajor at or above ZotFloor, as its tag (v2.1.23).
// Every other tag is skipped; the order of the list does not matter.
func PickZotRelease(tags []string) (string, bool) {
	var best *semver.Version
	for _, tag := range tags {
		v, ok := ParseZotRelease(tag)
		if !ok || v.LessThan(ZotFloor) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	if best == nil {
		return "", false
	}
	return best.Original(), true
}

// ZotImage is the zot image of a release tag.
func ZotImage(tag string) string { return ZotRepository + ":" + tag }

// ValidateZotVersion checks a zotVersion: empty (follow zot's releases) or a
// release tag of the major the lab follows. The floor is checked where the
// version is used (`configure --zot-version` and the lab registry's
// resolution), not here: a file an older agentlab wrote with a lower floor
// must stay loadable, or `configure --zot-version ""` could not mend it.
func ValidateZotVersion(tag string) error {
	if tag == "" {
		return nil
	}
	if _, ok := ParseZotRelease(tag); !ok {
		return fmt.Errorf("want a zot %d release tag of %s (v%d.<minor>.<patch>, no prerelease), or \"\" to follow zot's releases", ZotMajor, ZotRepository, ZotMajor)
	}
	return nil
}

// CheckZotVersion is ValidateZotVersion with the floor: what `agentlab
// configure --zot-version` takes, and what the lab registry runs.
func CheckZotVersion(tag string) error {
	if err := ValidateZotVersion(tag); err != nil || tag == "" {
		return err
	}
	if v, _ := ParseZotRelease(tag); v.LessThan(ZotFloor) {
		return fmt.Errorf("below v%s, the oldest zot with what agentlab's zot configurations use", ZotFloor)
	}
	return nil
}

// RegistryMirrorOn says whether the lab's nodes pull gsoci through the
// shared registry mirror.
func (c *Config) RegistryMirrorOn() bool {
	return c.RegistryMirror == nil || *c.RegistryMirror
}

// SetRegistryMirror records the lab's registry-mirror setting: on as no key
// at all.
func (c *Config) SetRegistryMirror(on bool) {
	c.RegistryMirror = nil
	if !on {
		c.RegistryMirror = new(false)
	}
}
