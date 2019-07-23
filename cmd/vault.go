package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/api"
	jww "github.com/spf13/jwalterweatherman"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var ErrPermissionDenied = errors.New("permission denied")

func defaultRetryStrategy(max time.Duration) backoff.BackOff {
	strategy := backoff.NewExponentialBackOff()
	strategy.InitialInterval = time.Millisecond * 500
	strategy.MaxElapsedTime = max
	return strategy
}

func revokeSelf(client *api.Client) {
	jww.DEBUG.Printf("Revoking Vault token.")
	err := client.Auth().Token().RevokeSelf(client.Token())
	if err != nil {
		jww.ERROR.Printf("Failed to revoke Vault token. This will leave credentials around in %q Vault: %v", client.Address(), err)
	}
}

func renewSelf(ctx context.Context, client *api.Client, duration time.Duration) error {
	jww.INFO.Print("Renewing Vault authentication token.")
	op := func() error {
		secret, err := client.Auth().Token().RenewSelf(int(duration.Seconds()))
		if err != nil {
			jww.ERROR.Printf("Error renewing authentication token: %v", err)
			if checkPermissionDenied(err) {
				return backoff.Permanent(ErrPermissionDenied)
			}
			return err
		}

		jww.INFO.Print("Vault authentication token renewed.")
		enrollAuthTokenInLease(secret)

		return nil
	}

	err := backoff.Retry(op, backoff.WithContext(defaultRetryStrategy(duration), ctx))

	return err
}

func performKubernetesAuth() (*api.Client, *api.Secret, error) {
	type login struct {
		JWT  string `json:"jwt"`
		Role string `json:"role"`
	}

	cfg := api.DefaultConfig()
	client, err := api.NewClient(cfg)
	if err != nil {
		jww.FATAL.Fatalf("Failed to create vault client to %q: %v", client.Address(), err)
	}

	jww.INFO.Printf("Reading Kubernetes service account token: %q", *serviceAccountToken)
	tokenBytes, err := ioutil.ReadFile(*serviceAccountToken)
	if err != nil {
		return nil, nil, err
	}

	jww.INFO.Printf("Authenticating to %q as role %q against %q", *k8sLoginPath, *k8sAuthRole, cfg.Address)

	req := client.NewRequest("POST", fmt.Sprintf("/v1/auth/%s/login", *k8sLoginPath))
	err = req.SetJSONBody(&login{JWT: string(tokenBytes), Role: *k8sAuthRole})
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.RawRequest(req)
	if err != nil {
		return nil, nil, err
	}

	if resp.Error() != nil {
		return nil, nil, resp.Error()
	}

	var secret api.Secret

	err = json.NewDecoder(resp.Body).Decode(&secret)
	if err != nil {
		return nil, nil, errwrap.Wrapf("error parsing response: {{err}}", err)
	}

	token, err := secret.TokenID()
	if err != nil {
		jww.FATAL.Fatalf("Could not extract Vault Token: %v", err)
	}

	client.SetToken(token)

	return client, &secret, nil
}

func performTokenAuth(cfg *api.Config, vaultToken string) (*api.Client, *api.Secret, error) {
	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, nil, err
	}

	client.SetToken(vaultToken)

	var secret *api.Secret
	secret, err = client.Auth().Token().LookupSelf()
	if err != nil {
		return nil, nil, err
	}

	jww.DEBUG.Printf("Token authentication to %q succeeded.", cfg.Address)
	return client, secret, nil
}

// Authenticate to the Vault server.
// 1. Use the token from the leases file if exists.
// 2. Use the token from --vault-token (if used)
// 3. Use VAULT_TOKEN if set.
// 4. Use K8s ServiceAccountToken against the k8s auth backend if specified.
func authenticateToVault() (*api.Client, *api.Secret, error) {

	// If there is a leases token, use it.
	if leases.AuthTokenLease.Token != "" {

		// DefaultConfig will digest VAULT_ environment variables
		cfg := api.DefaultConfig()

		jww.INFO.Printf("Logging into Vault server %q with token from lease", cfg.Address)
		client, secret, err := performTokenAuth(cfg, leases.AuthTokenLease.Token)

		if err != nil {
			jww.FATAL.Fatalf("Failed to authenticate to vault server %q with token in lease file. Leases will not be renewed. Error: %v",
				cfg.Address, err)
		}

		return client, secret, nil
	}

	// Check if -vault-token was passed in

	if *vaultTokenArg != "" {

		// DefaultConfig will digest VAULT_ environment variables
		cfg := api.DefaultConfig()

		jww.INFO.Printf("Logging into Vault server %q with command line token.", cfg.Address)

		client, secret, err := performTokenAuth(cfg, *vaultTokenArg)
		if err != nil {
			jww.FATAL.Fatalf("Failed to authenticate to Vault Server %q using command line token: %v", cfg.Address, err)
		}
		return client, secret, nil
	}

	// Otherwise, if VAULT_TOKEN is set, use that.

	vaultToken := os.Getenv(api.EnvVaultToken)

	if vaultToken != "" {

		// DefaultConfig will digest VAULT_ environment variables
		cfg := api.DefaultConfig()

		jww.INFO.Printf("Logging into Vault server %q with token in %q", cfg.Address, api.EnvVaultToken)

		client, secret, err := performTokenAuth(cfg, vaultToken)
		if err != nil {
			jww.FATAL.Fatalf("Failed to authenticate to Vault Server %q using %q: %v", cfg.Address,
				api.EnvVaultToken, err)
		}
		return client, secret, nil
	}

	// Otherwise, if there is a ConfigMap named vault-token in the default namespace, use the token it stores

	config, err := rest.InClusterConfig()
	// If we cannot create the in cluster config, that means we are not running inside of Kubernetes
	if err != nil {
		jww.DEBUG.Print("Could not create cluster config - this will fail if this is running outside of Kubernetes")
	} else {

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			jww.DEBUG.Print("Could not create clientset to call Kubernetes API")
		} else {

			configMaps, err := clientset.CoreV1().ConfigMaps("default").List(v1.ListOptions{FieldSelector: "metadata.name=vault-token"})
			if err != nil {
				jww.DEBUG.Printf("Failed to get configmaps filtered on the name vault-token: %v", err)
			} else if len(configMaps.Items) == 1 {
				if token, exists := configMaps.Items[0].Data["token"]; exists {
					// DefaultConfig will digest VAULT_ environment variables
					cfg := api.DefaultConfig()

					jww.INFO.Printf("Logging into Vault server %q with token from vault-token ConfigMap.", cfg.Address)

					client, secret, err := performTokenAuth(cfg, token)
					if err != nil {
						jww.FATAL.Fatalf("Failed to authenticate to Vault Server %q using token from vault-token ConfigMap: %v", cfg.Address, err)
					}
					return client, secret, nil
				}
			} else {
				jww.DEBUG.Print("Damn, multiple configmaps were returned when filtering configmaps with the name vault-token. How did this even happen?")
			}
		}
	}

	// Lastly, if there's a Kubernetes Auth Role setup, use that...

	if *k8sAuthRole != "" {
		client, secret, err := performKubernetesAuth()
		return client, secret, err
	}

	jww.FATAL.Fatalf("No authentication mechanism specified and %q is not set.", api.EnvVaultToken)
	return nil, nil, nil
}

func readKVSecrets(client *api.Client) map[string]api.Secret {

	var vaultSecretsMapping = make(map[string]api.Secret)

	for _, request := range config.Secrets {

		key := request.Key

		jww.INFO.Printf("Fetching secret: %q", request.Path)

		var path string

		if !strings.HasPrefix(request.Path, "/") {
			path = filepath.Join(*serviceSecretPrefix, request.Path)
		} else {
			path = request.Path
		}

		if _, ok := vaultSecretsMapping[key]; ok {
			jww.FATAL.Fatalf("Duplicate secret key %q.", key)
		}

		jww.DEBUG.Printf("Reading secrets from %q", path)
		response, err := client.Logical().Read(path)

		if err != nil {
			jww.FATAL.Fatalf("error fetching secret %q from %q: %v", path, client.Address(), err)
		}

		if response == nil {
			if request.IsMissingOk {
				jww.INFO.Printf("No response reading secrets from %q on path %q (either access is denied "+
					"or there are no secrets). Ignoring since missingOk is set in the config.",
					client.Address(), path)
			} else {
				jww.FATAL.Fatalf("No response returned fetching secrets.")
			}
		} else {
			enrollSecretInLease(response)
			vaultSecretsMapping[key] = *response
		}
	}

	return vaultSecretsMapping
}

func checkPermissionDenied(err error) bool {
	errorString := fmt.Sprintf("%s", err)
	return strings.Contains(errorString, "Code: 403")
}
