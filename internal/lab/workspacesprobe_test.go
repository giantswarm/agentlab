package lab

import (
	"maps"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// restrictedViolations lists what keeps the pod out of the PodSecurity
// `restricted` profile as Giant Swarm installations enforce it through
// Kyverno: require-run-as-nonroot, disallow-privilege-escalation,
// disallow-capabilities-strict, restrict-seccomp-strict and
// restrict-volume-types.
func restrictedViolations(pod *corev1.Pod) []string {
	var v []string
	psc := pod.Spec.SecurityContext
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil && vol.EmptyDir == nil && vol.ConfigMap == nil && vol.Secret == nil && vol.Projected == nil {
			v = append(v, "volume "+vol.Name+" is of a type restricted refuses")
		}
	}
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		sc := c.SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		nonRoot := sc.RunAsNonRoot
		if nonRoot == nil {
			nonRoot = psc.RunAsNonRoot
		}
		if nonRoot == nil || !*nonRoot {
			v = append(v, c.Name+": runAsNonRoot is not true")
		}
		user := sc.RunAsUser
		if user == nil {
			user = psc.RunAsUser
		}
		if user == nil || *user == 0 {
			v = append(v, c.Name+": no non-root runAsUser")
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			v = append(v, c.Name+": allowPrivilegeEscalation is not false")
		}
		if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, "ALL") {
			v = append(v, c.Name+": capabilities do not drop ALL")
		} else if len(sc.Capabilities.Add) > 0 {
			v = append(v, c.Name+": capabilities are added")
		}
		seccomp := sc.SeccompProfile
		if seccomp == nil {
			seccomp = psc.SeccompProfile
		}
		if seccomp == nil || (seccomp.Type != corev1.SeccompProfileTypeRuntimeDefault && seccomp.Type != corev1.SeccompProfileTypeLocalhost) {
			v = append(v, c.Name+": seccompProfile is not RuntimeDefault")
		}
		if sc.Privileged != nil && *sc.Privileged {
			v = append(v, c.Name+": privileged")
		}
	}
	return v
}

func TestWorkspaceVolumeProbesAreRestricted(t *testing.T) {
	gitMounts := []corev1.VolumeMount{
		{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, SubPath: workspacesTestSessions + "/s"},
		{Name: workspacesTestVolume, MountPath: workspacesActorMirrorsPath, SubPath: workspacesTestMirrors, ReadOnly: true},
		{Name: measureLocalVolume, MountPath: "/" + measureLocalVolume},
	}
	for name, pod := range map[string]*corev1.Pod{
		"measure dirs":  measureVolumePod("claim", "dirs", sessionsDirectoriesScript, nil, workspaceVolumeOwner),
		"measure clear": measureVolumePod("claim", "clear", measureClearSessionsScript(workspaceVolumeOwner), nil, workspaceVolumeOwner),
		"measure git":   measureVolumePod("claim", "git", measureGitScript("o/r", "main", ""), gitMounts, workspaceVolumeOwner),
		"sync clear":    workspaceVolumePod("agentlab-sync-clear", "claim", syncRemoveSessionScript),
	} {
		if v := restrictedViolations(pod); len(v) > 0 {
			t.Errorf("%s: not PodSecurity restricted: %v", name, v)
		}
		psc := pod.Spec.SecurityContext
		for field, got := range map[string]*int64{"runAsUser": psc.RunAsUser, "runAsGroup": psc.RunAsGroup, "fsGroup": psc.FSGroup} {
			if got == nil || *got != workspaceVolumeUser {
				t.Errorf("%s: %s is %v, want the sync Job's %d", name, field, got, workspaceVolumeUser)
			}
		}
		if env := pod.Spec.Containers[0].Env; len(env) != 1 || env[0].Name != "HOME" || env[0].Value != "/tmp" {
			t.Errorf("%s: env %+v, want HOME=/tmp: the user has no home, and git config --global writes there", name, env)
		}
		if pod.Spec.Volumes[0].PersistentVolumeClaim == nil || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "claim" {
			t.Errorf("%s: the first volume is not the workspace's claim: %+v", name, pod.Spec.Volumes[0])
		}
	}
	git := measureVolumePod("claim", "git", measureGitScript("o/r", "main", ""), gitMounts, workspaceVolumeOwner)
	if len(git.Spec.Volumes) != 2 || git.Spec.Volumes[1].EmptyDir == nil || len(git.Spec.Containers[0].VolumeMounts) != len(gitMounts) {
		t.Errorf("the git probe has volumes %+v and mounts %+v, want the claim, the local emptyDir and its mounts", git.Spec.Volumes, git.Spec.Containers[0].VolumeMounts)
	}
}

// The check itself refuses the probe pod as it was before the fix: no
// security context at all.
func TestRestrictedViolationsRefuseABarePod(t *testing.T) {
	pod := probePod("bare", workspacesTestGitImage, []string{"sh"})
	if v := restrictedViolations(pod); len(v) != 5 {
		t.Errorf("a bare probe pod has violations %v, want runAsNonRoot, runAsUser, privilege escalation, capabilities and seccomp", v)
	}
}

// A probe entering a Session's directory runs as the directory's owner (the
// runtime's actor where the volume keeps owner and mode, as Azure Files over
// NFS does), still restricted and with the sync Job's fsGroup.
func TestMeasureProbesRunAsSessionOwner(t *testing.T) {
	actor := volumeOwner{65532, 65532}
	for name, pod := range map[string]*corev1.Pod{
		"dirs":  measureVolumePod("claim", "dirs", sessionsDirectoriesScript, nil, actor),
		"clear": measureVolumePod("claim", "clear", measureClearSessionsScript(actor), nil, actor),
		"git":   measureVolumePod("claim", "git", measureGitScript("o/r", "main", ""), nil, actor),
	} {
		if v := restrictedViolations(pod); len(v) > 0 {
			t.Errorf("%s: not PodSecurity restricted: %v", name, v)
		}
		psc := pod.Spec.SecurityContext
		if *psc.RunAsUser != actor.uid || *psc.RunAsGroup != actor.gid {
			t.Errorf("%s: runs as %d:%d, want the directory's owner %d:%d", name, *psc.RunAsUser, *psc.RunAsGroup, actor.uid, actor.gid)
		}
		if *psc.FSGroup != workspaceVolumeUser {
			t.Errorf("%s: fsGroup %d, want the sync Job's %d", name, *psc.FSGroup, workspaceVolumeUser)
		}
	}
}

func TestParseSessionOwners(t *testing.T) {
	out := "owner a 65532 65532\nowner b 1000 1000\nowner c 0 0\nowner d x 1\nnoise\nowner e 65532 65532"
	owners := parseSessionOwners(out)
	want := map[string]volumeOwner{"a": {65532, 65532}, "b": {1000, 1000}, "c": {0, 0}, "e": {65532, 65532}}
	if !maps.Equal(owners, want) {
		t.Errorf("parseSessionOwners = %v, want %v", owners, want)
	}
	if got := distinctOwners(owners); !slices.Equal(got, []volumeOwner{{0, 0}, {1000, 1000}, {65532, 65532}}) {
		t.Errorf("distinctOwners = %v", got)
	}
	for o, want := range map[volumeOwner]volumeOwner{
		{65532, 65532}: {65532, 65532},
		{0, 0}:         workspaceVolumeOwner,
		{}:             workspaceVolumeOwner,
		{1001, 0}:      {1001, 0},
	} {
		if got := o.runnable(); got != want {
			t.Errorf("%v.runnable() = %v, want %v", o, got, want)
		}
	}
	if got := owners["missing"].runnable(); got != workspaceVolumeOwner {
		t.Errorf("an unread owner runs as %v, want the sync Job's", got)
	}
}
