package lab

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

func consentDeploymentDefaults(ctx context.Context) error {
	cluster, err := labKube()
	if err != nil {
		return err
	}
	for _, name := range []string{componentMuster, componentMCPKubernetes} {
		deployment, err := cluster.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			for _, argument := range container.Args {
				if strings.Contains(strings.ToLower(argument), "consent") || strings.Contains(argument, "trusted-client") {
					return fmt.Errorf("%s sets a consent or trusted-client argument", name)
				}
			}
			for _, variable := range container.Env {
				if strings.Contains(variable.Name, "CONSENT") || strings.Contains(variable.Name, "TRUSTED_CLIENT") {
					return fmt.Errorf("%s sets a consent or trusted-client environment variable", name)
				}
			}
		}
		note("%s: no consent flag, trust entry or consent key override", name)
	}
	return nil
}

func (flow *consentProofFlow) checkReplicaReplacement(ctx context.Context, configuration *config.Config, user *config.User, name, base, accessToken string) error {
	if name == componentMuster && configuration.MusterReplicas() < 2 {
		return fmt.Errorf("the replica proof needs the lab's two-replica pod-network topology")
	}
	_, form, err := flow.page(ctx, "consent")
	if err != nil {
		return err
	}
	approved, err := flow.answer(ctx, form, consentOrigin(flow.metadata.Authorization))
	if err := expectConsentStatus(approved, err, http.StatusFound); err != nil {
		return err
	}
	flow.upstream = approved.header.Get("Location")
	cluster, err := labKube()
	if err != nil {
		return err
	}
	deployment, err := cluster.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	pods, err := deploymentPods(ctx, cluster, deployment)
	if err != nil {
		return err
	}
	if len(pods) != 2 {
		return fmt.Errorf("expected two %s pods, got %d", name, len(pods))
	}
	for _, pod := range pods {
		current, err := deploymentPods(ctx, cluster, deployment)
		if err != nil {
			return err
		}
		old := map[string]bool{}
		for _, existing := range current {
			old[existing.Name] = true
		}
		if err := cluster.clientset.CoreV1().Pods(platformNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if _, err := waitReplacementReady(ctx, cluster, deployment, old, 2*time.Minute); err != nil {
			return err
		}
		note("replaced %s while the approved upstream flow remained pending", pod.Name)
	}
	if err := consentMCPRead(ctx, configuration, user, base, accessToken, name == componentMuster); err != nil {
		return fmt.Errorf("existing issued token after replica replacement: %w", err)
	}
	code, err := flow.authenticate(ctx, user)
	if err != nil {
		return err
	}
	issuedToken, err := flow.exchange(ctx, code)
	if err != nil {
		return err
	}
	return consentMCPRead(ctx, configuration, user, base, issuedToken, name == componentMuster)
}
