// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

const (
	ngcContainerRegistryHost = "nvcr.io"
	ngcHelmRepositoryHost    = "helm.ngc.nvidia.com"
)

// helmRepositoryCredential is kept private so repository credentials cannot
// accidentally become part of config serialization or user-facing output.
// sourceSecret is safe to log; Username and Password must never be logged.
type helmRepositoryCredential struct {
	Username     string
	Password     string
	SourceSecret string
}

type dockerConfigJSON struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// loadHelmRepositoryCredentials reads the configured image pull Secrets from
// the Network Operator namespace. Kubernetes imagePullSecrets are not
// automatically consulted by Helm, but an NGC docker-registry Secret carries
// the same $oauthtoken/API-key pair used by helm.ngc.nvidia.com.
func loadHelmRepositoryCredentials(
	ctx context.Context,
	restConfig *rest.Config,
	cfg *config.NetworkOperatorConfig,
	namespace string,
) ([]helmRepositoryCredential, error) {
	if cfg == nil || len(cfg.ImagePullSecrets) == 0 {
		return nil, nil
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client for image pull secrets: %w", err)
	}

	return credentialsFromImagePullSecrets(
		ctx,
		clientset.CoreV1().Secrets(namespace),
		cfg.ImagePullSecrets,
		cfg.HelmRepoURL,
		namespace,
	)
}

// credentialsFromImagePullSecrets returns credentials in configured Secret
// order. Credentials are accepted only for the exact chart repository host,
// plus nvcr.io when the chart repository is helm.ngc.nvidia.com. The latter is
// an intentional NVIDIA-specific mapping: both endpoints use the same NGC
// $oauthtoken/API-key credential. Never forward credentials for an unrelated
// image registry to the chart host.
func credentialsFromImagePullSecrets(
	ctx context.Context,
	secrets corev1client.SecretInterface,
	secretNames []string,
	helmRepoURL string,
	namespace string,
) ([]helmRepositoryCredential, error) {
	targetHosts, err := helmCredentialHosts(helmRepoURL)
	if err != nil {
		return nil, err
	}

	credentials := make([]helmRepositoryCredential, 0, len(secretNames))
	var errs []error
	for _, secretName := range secretNames {
		secret, getErr := secrets.Get(ctx, secretName, metav1.GetOptions{})
		if getErr != nil {
			errs = append(errs, fmt.Errorf(
				"get image pull secret %q in namespace %q: %w",
				secretName, namespace, getErr))
			continue
		}

		auths, parseErr := dockerAuthsFromSecret(secret)
		if parseErr != nil {
			errs = append(errs, fmt.Errorf(
				"parse image pull secret %q in namespace %q: %w",
				secretName, namespace, parseErr))
			continue
		}

		credential, found, credentialErr := matchingDockerCredential(auths, targetHosts)
		if credentialErr != nil {
			errs = append(errs, fmt.Errorf(
				"parse credentials in image pull secret %q in namespace %q: %w",
				secretName, namespace, credentialErr))
			continue
		}
		if !found {
			// The Secret may be required by a component image from another
			// registry. It is valid for values.yaml but must not be sent to
			// an unrelated Helm repository.
			continue
		}

		credential.SourceSecret = secretName
		credentials = append(credentials, credential)
	}

	// Match kubelet's ordered imagePullSecrets behavior: one usable Secret is
	// sufficient even when another configured Secret is absent or malformed.
	// If none can authenticate this repository, surface the lookup/parse
	// failures instead of letting Helm fail later with an opaque HTTP 401.
	if len(credentials) > 0 {
		return credentials, nil
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, nil
}

func helmCredentialHosts(repoURL string) ([]string, error) {
	host, err := registryHost(repoURL)
	if err != nil {
		return nil, fmt.Errorf("parse Helm repository URL %q: %w", repoURL, err)
	}
	hosts := []string{host}
	if host == ngcHelmRepositoryHost {
		hosts = append(hosts, ngcContainerRegistryHost)
	}
	return hosts, nil
}

func dockerAuthsFromSecret(secret *corev1.Secret) (map[string]dockerAuthConfig, error) {
	if secret == nil {
		return nil, errors.New("secret is nil")
	}

	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var cfg dockerConfigJSON
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode %s: %w", corev1.DockerConfigJsonKey, err)
		}
		if len(cfg.Auths) == 0 {
			return nil, fmt.Errorf("%s contains no auth entries", corev1.DockerConfigJsonKey)
		}
		return cfg.Auths, nil
	}

	if raw, ok := secret.Data[corev1.DockerConfigKey]; ok {
		var auths map[string]dockerAuthConfig
		if err := json.Unmarshal(raw, &auths); err != nil {
			return nil, fmt.Errorf("decode %s: %w", corev1.DockerConfigKey, err)
		}
		if len(auths) == 0 {
			return nil, fmt.Errorf("%s contains no auth entries", corev1.DockerConfigKey)
		}
		return auths, nil
	}

	return nil, fmt.Errorf("secret has neither %s nor %s data", corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

func matchingDockerCredential(
	auths map[string]dockerAuthConfig,
	targetHosts []string,
) (helmRepositoryCredential, bool, error) {
	for _, targetHost := range targetHosts {
		for registry, auth := range auths {
			host, err := registryHost(registry)
			if err != nil || host != targetHost {
				continue
			}
			credential, err := credentialFromDockerAuth(auth)
			if err != nil {
				return helmRepositoryCredential{}, false,
					fmt.Errorf("registry %q: %w", registry, err)
			}
			return credential, true, nil
		}
	}
	return helmRepositoryCredential{}, false, nil
}

func credentialFromDockerAuth(auth dockerAuthConfig) (helmRepositoryCredential, error) {
	if auth.Username != "" || auth.Password != "" {
		if auth.Username == "" || auth.Password == "" {
			return helmRepositoryCredential{}, errors.New("username and password must both be set")
		}
		return helmRepositoryCredential{Username: auth.Username, Password: auth.Password}, nil
	}

	if auth.Auth == "" {
		return helmRepositoryCredential{}, errors.New("auth entry has no username/password or auth token")
	}
	decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
	if err != nil {
		return helmRepositoryCredential{}, fmt.Errorf("decode auth token: %w", err)
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok || username == "" || password == "" {
		return helmRepositoryCredential{}, errors.New("decoded auth token is not username:password")
	}
	return helmRepositoryCredential{Username: username, Password: password}, nil
}

func registryHost(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("registry URL is empty")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", errors.New("registry URL has no host")
	}
	return strings.ToLower(parsed.Host), nil
}
