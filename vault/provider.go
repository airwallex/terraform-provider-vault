package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/helper/logging"
	"github.com/hashicorp/terraform-plugin-sdk/helper/mutexkv"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/terraform"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/command/config"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iam/v1"
)

const (
	// GenericPath is used for inventorying paths that can be used for
	// multiple endpoints in Vault.
	GenericPath = "generic"

	// UnknownPath is used for inventorying paths that have no obvious
	// current endpoint they serve in Vault, and may relate to previous
	// versions of Vault.
	// We aim to deprecate items in this category.
	UnknownPath = "unknown"
)

// This is a global MutexKV for use within this provider.
// Use this when you need to have multiple resources or even multiple instances
// of the same resource write to the same path in Vault.
// The key of the mutex should be the path in Vault.
var vaultMutexKV = mutexkv.NewMutexKV()

func getSignedJWT(project string, serviceAccount string, creds string, role string) string {
	config, err := google.JWTConfigFromJSON([]byte(creds), iam.CloudPlatformScope)
	if err != nil {
		log.Fatal(err)
	}

	httpClient := config.Client(oauth2.NoContext)
	iamClient, err := iam.New(httpClient)
	if err != nil {
		log.Fatal(err)
	}
	// End [GCP IAM Setup]

	// 1. Generate signed JWT using IAM.
	resourceName := fmt.Sprintf("projects/%s/serviceAccounts/%s", project, serviceAccount)
	jwtPayload := map[string]interface{}{
		"aud": role,
		"sub": serviceAccount,
		"exp": time.Now().Add(time.Minute * 10).Unix(),
	}

	payloadBytes, err := json.Marshal(jwtPayload)
	if err != nil {
		log.Fatal(err)
	}
	signJwtReq := &iam.SignJwtRequest{
		Payload: string(payloadBytes),
	}

	resp, err := iamClient.Projects.ServiceAccounts.SignJwt(
		resourceName, signJwtReq).Do()
	if err != nil {
		log.Fatal(err)
	}

	return resp.SignedJwt
}

func Provider() terraform.ResourceProvider {
	dataSourcesMap, err := parse(DataSourceRegistry)
	if err != nil {
		panic(err)
	}
	resourcesMap, err := parse(ResourceRegistry)
	if err != nil {
		panic(err)
	}
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"address": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_ADDR", nil),
				Description: "URL of the root of the target Vault server.",
			},
			"token": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_TOKEN", ""),
				Description: "Token to use to authenticate to Vault.",
			},
			"ca_cert_file": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_CACERT", ""),
				Description: "Path to a CA certificate file to validate the server's certificate.",
			},
			"ca_cert_dir": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_CAPATH", ""),
				Description: "Path to directory containing CA certificate files to validate the server's certificate.",
			},
			"auth_login": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Login to vault with an existing auth method using auth/<mount>/login",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"method": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"project": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"service_account": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"creds": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"path": {
							Type:     schema.TypeString,
							Required: true,
						},
						"namespace": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"parameters": {
							Type:     schema.TypeMap,
							Optional: true,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
					},
				},
			},
			"client_auth": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Client authentication credentials.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"cert_file": {
							Type:        schema.TypeString,
							Required:    true,
							DefaultFunc: schema.EnvDefaultFunc("VAULT_CLIENT_CERT", ""),
							Description: "Path to a file containing the client certificate.",
						},
						"key_file": {
							Type:        schema.TypeString,
							Required:    true,
							DefaultFunc: schema.EnvDefaultFunc("VAULT_CLIENT_KEY", ""),
							Description: "Path to a file containing the private key that the certificate was issued for.",
						},
					},
				},
			},
			"skip_tls_verify": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_SKIP_VERIFY", false),
				Description: "Set this to true only if the target Vault server is an insecure development instance.",
			},
			"max_lease_ttl_seconds": {
				Type:     schema.TypeInt,
				Optional: true,

				// Default is 20min, which is intended to be enough time for
				// a reasonable Terraform run can complete but not
				// significantly longer, so that any leases are revoked shortly
				// after Terraform has finished running.
				DefaultFunc: schema.EnvDefaultFunc("TERRAFORM_VAULT_MAX_TTL", 1200),

				Description: "Maximum TTL for secret leases requested by this provider",
			},
			"max_retries": {
				Type:     schema.TypeInt,
				Optional: true,

				DefaultFunc: schema.EnvDefaultFunc("VAULT_MAX_RETRIES", 2),
				Description: "Maximum number of retries when a 5xx error code is encountered.",
			},
			"namespace": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("VAULT_NAMESPACE", ""),
				Description: "The namespace to use. Available only for Vault Enterprise",
			},
		},
		ConfigureFunc:  providerConfigure,
		DataSourcesMap: dataSourcesMap,
		ResourcesMap:   resourcesMap,
	}
}

// Description is essentially a DataSource or Resource with some additional metadata
// that helps with maintaining the Terraform Vault Provider.
type Description struct {
	// PathInventory is used for taking an inventory of the supported endpoints in the
	// Terraform Vault Provider and comparing them to the endpoints noted as available in
	// Vault's OpenAPI description. A list of Vault's endpoints can be obtained by,
	// from Vault's home directory, running "$ ./scripts/gen_openapi.sh", and then by
	// drilling into the paths with "$ cat openapi.json | jq ".paths" | jq 'keys[]'".
	// Here's a short example of how paths and their path variables should be represented:
	//		"/transit/keys/{name}/config"
	//		"/transit/random"
	//		"/transit/random/{urlbytes}"
	//		"/transit/sign/{name}/{urlalgorithm}"
	PathInventory []string

	// EnterpriseOnly defaults to false, but should be marked true if a resource is enterprise only.
	EnterpriseOnly bool

	Resource *schema.Resource
}

var (
	DataSourceRegistry = map[string]*Description{
		"vault_approle_auth_backend_role_id": {
			Resource:      approleAuthBackendRoleIDDataSource(),
			PathInventory: []string{"/auth/approle/role/{role_name}/role-id"},
		},
		"vault_identity_entity": {
			Resource:      identityEntityDataSource(),
			PathInventory: []string{"/identity/lookup/entity"},
		},
		"vault_identity_group": {
			Resource:      identityGroupDataSource(),
			PathInventory: []string{"/identity/lookup/group"},
		},
		"vault_kubernetes_auth_backend_config": {
			Resource:      kubernetesAuthBackendConfigDataSource(),
			PathInventory: []string{"/auth/kubernetes/config"},
		},
		"vault_kubernetes_auth_backend_role": {
			Resource:      kubernetesAuthBackendRoleDataSource(),
			PathInventory: []string{"/auth/kubernetes/role/{name}"},
		},
		"vault_aws_access_credentials": {
			Resource:      awsAccessCredentialsDataSource(),
			PathInventory: []string{"/aws/creds"},
		},
		"vault_generic_secret": {
			Resource:      genericSecretDataSource(),
			PathInventory: []string{"/secret/data/{path}"},
		},
		"vault_policy_document": {
			Resource:      policyDocumentDataSource(),
			PathInventory: []string{"/sys/policy/{name}"},
		},
	}

	ResourceRegistry = map[string]*Description{
		"vault_approle_auth_backend_login": {
			Resource:      approleAuthBackendLoginResource(),
			PathInventory: []string{"/auth/approle/login"},
		},
		"vault_approle_auth_backend_role": {
			Resource:      approleAuthBackendRoleResource(),
			PathInventory: []string{"/auth/approle/role/{role_name}"},
		},
		"vault_approle_auth_backend_role_secret_id": {
			Resource: approleAuthBackendRoleSecretIDResource(),
			PathInventory: []string{
				"/auth/approle/role/{role_name}/secret-id",
				"/auth/approle/role/{role_name}/custom-secret-id",
			},
		},
		"vault_auth_backend": {
			Resource:      authBackendResource(),
			PathInventory: []string{"/sys/auth/{path}"},
		},
		"vault_token": {
			Resource: tokenResource(),
			PathInventory: []string{
				"/auth/token/create",
				"/auth/token/create-orphan",
				"/auth/token/create/{role_name}",
			},
		},
		"vault_token_auth_backend_role": {
			Resource:      tokenAuthBackendRoleResource(),
			PathInventory: []string{"/auth/token/roles/{role_name}"},
		},
		"vault_aws_auth_backend_cert": {
			Resource:      awsAuthBackendCertResource(),
			PathInventory: []string{"/auth/aws/config/certificate/{cert_name}"},
		},
		"vault_aws_auth_backend_client": {
			Resource:      awsAuthBackendClientResource(),
			PathInventory: []string{"/auth/aws/config/client"},
		},
		"vault_aws_auth_backend_identity_whitelist": {
			Resource:      awsAuthBackendIdentityWhitelistResource(),
			PathInventory: []string{"/auth/aws/config/tidy/identity-whitelist"},
		},
		"vault_aws_auth_backend_login": {
			Resource:      awsAuthBackendLoginResource(),
			PathInventory: []string{"/auth/aws/login"},
		},
		"vault_aws_auth_backend_role": {
			Resource:      awsAuthBackendRoleResource(),
			PathInventory: []string{"/auth/aws/role/{role}"},
		},
		"vault_aws_auth_backend_role_tag": {
			Resource:      awsAuthBackendRoleTagResource(),
			PathInventory: []string{"/auth/aws/role/{role}/tag"},
		},
		"vault_aws_auth_backend_roletag_blacklist": {
			Resource:      awsAuthBackendRoleTagBlacklistResource(),
			PathInventory: []string{"/auth/aws/config/tidy/roletag-blacklist"},
		},
		"vault_aws_auth_backend_sts_role": {
			Resource:      awsAuthBackendSTSRoleResource(),
			PathInventory: []string{"/auth/aws/config/sts/{account_id}"},
		},
		"vault_aws_secret_backend": {
			Resource:      awsSecretBackendResource(),
			PathInventory: []string{"/aws/config/root"},
		},
		"vault_aws_secret_backend_role": {
			Resource:      awsSecretBackendRoleResource(),
			PathInventory: []string{"/aws/roles/{name}"},
		},
		"vault_azure_secret_backend": {
			Resource:      azureSecretBackendResource(),
			PathInventory: []string{"/azure/config"},
		},
		"vault_azure_secret_backend_role": {
			Resource:      azureSecretBackendRoleResource(),
			PathInventory: []string{"/azure/roles/{name}"},
		},
		"vault_azure_auth_backend_config": {
			Resource:      azureAuthBackendConfigResource(),
			PathInventory: []string{"/auth/azure/config"},
		},
		"vault_azure_auth_backend_role": {
			Resource:      azureAuthBackendRoleResource(),
			PathInventory: []string{"/auth/azure/role/{name}"},
		},
		"vault_consul_secret_backend": {
			Resource:      consulSecretBackendResource(),
			PathInventory: []string{"/consul/config/access"},
		},
		"vault_consul_secret_backend_role": {
			Resource:      consulSecretBackendRoleResource(),
			PathInventory: []string{"/consul/roles/{name}"},
		},
		"vault_database_secret_backend_connection": {
			Resource:      databaseSecretBackendConnectionResource(),
			PathInventory: []string{"/database/config/{name}"},
		},
		"vault_database_secret_backend_role": {
			Resource:      databaseSecretBackendRoleResource(),
			PathInventory: []string{"/database/roles/{name}"},
		},
		"vault_database_secret_backend_static_role": {
			Resource:      databaseSecretBackendStaticRoleResource(),
			PathInventory: []string{"/database/static-roles/{name}"},
		},
		"vault_github_auth_backend": {
			Resource:      githubAuthBackendResource(),
			PathInventory: []string{"/auth/github/config"},
		},
		"vault_github_team": {
			Resource:      githubTeamResource(),
			PathInventory: []string{"/auth/github/map/teams"},
		},
		"vault_github_user": {
			Resource:      githubUserResource(),
			PathInventory: []string{"/auth/github/map/users"},
		},
		"vault_gcp_auth_backend": {
			Resource:      gcpAuthBackendResource(),
			PathInventory: []string{"/auth/gcp/config"},
		},
		"vault_gcp_auth_backend_role": {
			Resource:      gcpAuthBackendRoleResource(),
			PathInventory: []string{"/auth/gcp/role/{name}"},
		},
		"vault_gcp_secret_backend": {
			Resource:      gcpSecretBackendResource(),
			PathInventory: []string{"/gcp/config"},
		},
		"vault_gcp_secret_roleset": {
			Resource:      gcpSecretRolesetResource(),
			PathInventory: []string{"/gcp/roleset/{name}"},
		},
		"vault_cert_auth_backend_role": {
			Resource:      certAuthBackendRoleResource(),
			PathInventory: []string{"/auth/cert/certs/{name}"},
		},
		"vault_generic_endpoint": {
			Resource:      genericEndpointResource(),
			PathInventory: []string{GenericPath},
		},
		"vault_generic_secret": {
			Resource:      genericSecretResource(),
			PathInventory: []string{GenericPath},
		},
		"vault_jwt_auth_backend": {
			Resource:      jwtAuthBackendResource(),
			PathInventory: []string{"/auth/jwt/config"},
		},
		"vault_jwt_auth_backend_role": {
			Resource:      jwtAuthBackendRoleResource(),
			PathInventory: []string{"/auth/jwt/role/{name}"},
		},
		"vault_kubernetes_auth_backend_config": {
			Resource:      kubernetesAuthBackendConfigResource(),
			PathInventory: []string{"/auth/kubernetes/config"},
		},
		"vault_kubernetes_auth_backend_role": {
			Resource:      kubernetesAuthBackendRoleResource(),
			PathInventory: []string{"/auth/kubernetes/role/{name}"},
		},
		"vault_okta_auth_backend": {
			Resource:      oktaAuthBackendResource(),
			PathInventory: []string{"/auth/okta/config"},
		},
		"vault_okta_auth_backend_user": {
			Resource:      oktaAuthBackendUserResource(),
			PathInventory: []string{"/auth/okta/users/{name}"},
		},
		"vault_okta_auth_backend_group": {
			Resource:      oktaAuthBackendGroupResource(),
			PathInventory: []string{"/auth/okta/groups/{name}"},
		},
		"vault_ldap_auth_backend": {
			Resource:      ldapAuthBackendResource(),
			PathInventory: []string{"/auth/ldap/config"},
		},
		"vault_ldap_auth_backend_user": {
			Resource:      ldapAuthBackendUserResource(),
			PathInventory: []string{"/auth/ldap/users/{name}"},
		},
		"vault_ldap_auth_backend_group": {
			Resource:      ldapAuthBackendGroupResource(),
			PathInventory: []string{"/auth/ldap/groups/{name}"},
		},
		"vault_policy": {
			Resource:      policyResource(),
			PathInventory: []string{"/sys/policy/{name}"},
		},
		"vault_egp_policy": {
			Resource:       egpPolicyResource(),
			PathInventory:  []string{"/sys/policies/egp/{name}"},
			EnterpriseOnly: true,
		},
		"vault_rgp_policy": {
			Resource:       rgpPolicyResource(),
			PathInventory:  []string{"/sys/policies/rgp/{name}"},
			EnterpriseOnly: true,
		},
		"vault_mfa_duo": {
			Resource:       mfaDuoResource(),
			PathInventory:  []string{"/sys/mfa/method/duo/{name}"},
			EnterpriseOnly: true,
		},
		"vault_mount": {
			Resource:      mountResource(),
			PathInventory: []string{"/sys/mounts/{path}"},
		},
		"vault_namespace": {
			Resource:       namespaceResource(),
			PathInventory:  []string{"/sys/namespaces/{path}"},
			EnterpriseOnly: true,
		},
		"vault_audit": {
			Resource:      auditResource(),
			PathInventory: []string{"/sys/audit/{path}"},
		},
		"vault_ssh_secret_backend_ca": {
			Resource:      sshSecretBackendCAResource(),
			PathInventory: []string{"/ssh/config/ca"},
		},
		"vault_ssh_secret_backend_role": {
			Resource:      sshSecretBackendRoleResource(),
			PathInventory: []string{"/ssh/roles/{role}"},
		},
		"vault_identity_entity": {
			Resource:      identityEntityResource(),
			PathInventory: []string{"/identity/entity"},
		},
		"vault_identity_entity_alias": {
			Resource:      identityEntityAliasResource(),
			PathInventory: []string{"/identity/entity-alias"},
		},
		"vault_identity_entity_policies": {
			Resource:      identityEntityPoliciesResource(),
			PathInventory: []string{"/identity/lookup/entity"},
		},
		"vault_identity_group": {
			Resource:      identityGroupResource(),
			PathInventory: []string{"/identity/group"},
		},
		"vault_identity_group_alias": {
			Resource:      identityGroupAliasResource(),
			PathInventory: []string{"/identity/group-alias"},
		},
		"vault_identity_group_policies": {
			Resource:      identityGroupPoliciesResource(),
			PathInventory: []string{"/identity/lookup/group"},
		},
		"vault_identity_oidc": {
			Resource:      identityOidc(),
			PathInventory: []string{"/identity/oidc/config"},
		},
		"vault_identity_oidc_key": {
			Resource:      identityOidcKey(),
			PathInventory: []string{"/identity/oidc/key/{name}"},
		},
		"vault_identity_oidc_key_allowed_client_id": {
			Resource:      identityOidcKeyAllowedClientId(),
			PathInventory: []string{"/identity/oidc/key/{key_name}"},
		},
		"vault_identity_oidc_role": {
			Resource:      identityOidcRole(),
			PathInventory: []string{"/identity/oidc/role/{name}"},
		},
		"vault_rabbitmq_secret_backend": {
			Resource: rabbitmqSecretBackendResource(),
			PathInventory: []string{
				"/rabbitmq/config/connection",
				"/rabbitmq/config/lease",
			},
		},
		"vault_rabbitmq_secret_backend_role": {
			Resource:      rabbitmqSecretBackendRoleResource(),
			PathInventory: []string{"/rabbitmq/roles/{name}"},
		},
		"vault_pki_secret_backend": {
			Resource:      pkiSecretBackendResource(),
			PathInventory: []string{UnknownPath},
		},
		"vault_pki_secret_backend_cert": {
			Resource:      pkiSecretBackendCertResource(),
			PathInventory: []string{"/pki/issue/{role}"},
		},
		"vault_pki_secret_backend_crl_config": {
			Resource:      pkiSecretBackendCrlConfigResource(),
			PathInventory: []string{"/pki/config/crl"},
		},
		"vault_pki_secret_backend_config_ca": {
			Resource:      pkiSecretBackendConfigCAResource(),
			PathInventory: []string{"/pki/config/ca"},
		},
		"vault_pki_secret_backend_config_urls": {
			Resource:      pkiSecretBackendConfigUrlsResource(),
			PathInventory: []string{"/pki/config/urls"},
		},
		"vault_pki_secret_backend_intermediate_cert_request": {
			Resource:      pkiSecretBackendIntermediateCertRequestResource(),
			PathInventory: []string{"/pki/intermediate/generate/{exported}"},
		},
		"vault_pki_secret_backend_intermediate_set_signed": {
			Resource:      pkiSecretBackendIntermediateSetSignedResource(),
			PathInventory: []string{"/pki/intermediate/set-signed"},
		},
		"vault_pki_secret_backend_role": {
			Resource:      pkiSecretBackendRoleResource(),
			PathInventory: []string{"/pki/roles/{name}"},
		},
		"vault_pki_secret_backend_root_cert": {
			Resource:      pkiSecretBackendRootCertResource(),
			PathInventory: []string{"/pki/root/generate/{exported}"},
		},
		"vault_pki_secret_backend_root_sign_intermediate": {
			Resource:      pkiSecretBackendRootSignIntermediateResource(),
			PathInventory: []string{"/pki/root/sign-intermediate"},
		},
		"vault_pki_secret_backend_sign": {
			Resource:      pkiSecretBackendSignResource(),
			PathInventory: []string{"/pki/sign/{role}"},
		},
		"vault_transit_secret_backend_key": {
			Resource:      transitSecretBackendKeyResource(),
			PathInventory: []string{"/transit/keys/{name}"},
		},
		"vault_transit_secret_cache_config": {
			Resource:      transitSecretBackendCacheConfig(),
			PathInventory: []string{"/transit/cache-config"},
		},
	}
)

func providerToken(d *schema.ResourceData) (string, error) {
	if token := d.Get("token").(string); token != "" {
		return token, nil
	}
	// Use ~/.vault-token, or the configured token helper.
	tokenHelper, err := config.DefaultTokenHelper()
	if err != nil {
		return "", fmt.Errorf("error getting token helper: %s", err)
	}
	token, err := tokenHelper.Get()
	if err != nil {
		return "", fmt.Errorf("error getting token: %s", err)
	}
	return strings.TrimSpace(token), nil
}

func providerConfigure(d *schema.ResourceData) (interface{}, error) {
	clientConfig := api.DefaultConfig()
	addr := d.Get("address").(string)
	if addr != "" {
		clientConfig.Address = addr
	}

	clientAuthI := d.Get("client_auth").([]interface{})
	if len(clientAuthI) > 1 {
		return nil, fmt.Errorf("client_auth block may appear only once")
	}

	clientAuthCert := ""
	clientAuthKey := ""
	if len(clientAuthI) == 1 {
		clientAuth := clientAuthI[0].(map[string]interface{})
		clientAuthCert = clientAuth["cert_file"].(string)
		clientAuthKey = clientAuth["key_file"].(string)
	}

	err := clientConfig.ConfigureTLS(&api.TLSConfig{
		CACert:   d.Get("ca_cert_file").(string),
		CAPath:   d.Get("ca_cert_dir").(string),
		Insecure: d.Get("skip_tls_verify").(bool),

		ClientCert: clientAuthCert,
		ClientKey:  clientAuthKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to configure TLS for Vault API: %s", err)
	}

	clientConfig.HttpClient.Transport = logging.NewTransport("Vault", clientConfig.HttpClient.Transport)

	client, err := api.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to configure Vault API: %s", err)
	}

	client.SetMaxRetries(d.Get("max_retries").(int))

	// Try an get the token from the config or token helper
	token, err := providerToken(d)
	if err != nil {
		return nil, err
	}

	// Attempt to use auth/<mount>login if 'auth_login' is provided in provider config
	authLoginI := d.Get("auth_login").([]interface{})
	if len(authLoginI) > 1 {
		return "", fmt.Errorf("auth_login block may appear only once")
	}

	if len(authLoginI) == 1 {
		authLogin := authLoginI[0].(map[string]interface{})
		authLoginPath := authLogin["path"].(string)
		authLoginNamespace := ""
		if authLoginNamespaceI, ok := authLogin["namespace"]; ok {
			authLoginNamespace = authLoginNamespaceI.(string)
			client.SetNamespace(authLoginNamespace)
		}
		authLoginParameters := authLogin["parameters"].(map[string]interface{})
		authLoginMethod := ""
		if authLoginMethodI, ok := authLogin["method"]; ok {
			authLoginMethod = authLoginMethodI.(string)
			if "gcp" == strings.ToLower(authLoginMethod) {
				project := ""
				if projectI, ok := authLogin["project"]; ok {
					project = projectI.(string)
				} else {
					log.Fatal("project is required when gcp method is specified")
				}
				serviceAccount := ""
				if serviceAccountI, ok := authLogin["service_account"]; ok {
					serviceAccount = serviceAccountI.(string)
				} else {
					log.Fatal("service_account is required when gcp method is specified")
				}
				creds := ""
				if credsI, ok := authLogin["creds"]; ok {
					creds = credsI.(string)
				} else {
					log.Fatal("creds is required when gcp method is specified")
				}
				role := ""
				if roleI, ok := authLoginParameters["role"]; ok {
					role = roleI.(string)
					role = "vault/" + role
				} else {
					log.Fatal("role is required when gcp method is specified")
				}
				signedJwt := getSignedJWT(project, serviceAccount, creds, role)
				log.Printf("[INFO] signedJwt: %s", signedJwt)
				authLoginParameters["jwt"] = signedJwt
			}
		}

		secret, err := client.Logical().Write(authLoginPath, authLoginParameters)
		if err != nil {
			return nil, err
		}
		token = secret.Auth.ClientToken
	}
	if token != "" {
		client.SetToken(token)
	}
	if client.Token() == "" {
		return nil, errors.New("no vault token found")
	}

	// In order to enforce our relatively-short lease TTL, we derive a
	// temporary child token that inherits all of the policies of the
	// token we were given but expires after max_lease_ttl_seconds.
	//
	// The intent here is that Terraform will need to re-fetch any
	// secrets on each run and so we limit the exposure risk of secrets
	// that end up stored in the Terraform state, assuming that they are
	// credentials that Vault is able to revoke.
	//
	// Caution is still required with state files since not all secrets
	// can explicitly be revoked, and this limited scope won't apply to
	// any secrets that are *written* by Terraform to Vault.

	// Set the namespace to the token's namespace only for the
	// child token creation
	tokenInfo, err := client.Auth().Token().LookupSelf()
	if err != nil {
		return nil, err
	}
	if tokenNamespaceRaw, ok := tokenInfo.Data["namespace_path"]; ok {
		tokenNamespace := tokenNamespaceRaw.(string)
		if tokenNamespace != "" {
			client.SetNamespace(tokenNamespace)
		}
	}

	renewable := false
	childTokenLease, err := client.Auth().Token().Create(&api.TokenCreateRequest{
		DisplayName:    "terraform",
		TTL:            fmt.Sprintf("%ds", d.Get("max_lease_ttl_seconds").(int)),
		ExplicitMaxTTL: fmt.Sprintf("%ds", d.Get("max_lease_ttl_seconds").(int)),
		Renewable:      &renewable,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create limited child token: %s", err)
	}

	childToken := childTokenLease.Auth.ClientToken
	policies := childTokenLease.Auth.Policies

	log.Printf("[INFO] Using Vault token with the following policies: %s", strings.Join(policies, ", "))

	// Set tht token to the generated child token
	client.SetToken(childToken)

	// Set the namespace to the requested namespace, if provided
	namespace := d.Get("namespace").(string)
	if namespace != "" {
		client.SetNamespace(namespace)
	}
	return client, nil
}

func parse(descs map[string]*Description) (map[string]*schema.Resource, error) {
	var errs error
	resourceMap := make(map[string]*schema.Resource)
	for k, desc := range descs {
		resourceMap[k] = desc.Resource
		if len(desc.PathInventory) == 0 {
			errs = multierror.Append(errs, fmt.Errorf("%q needs its paths inventoried", k))
		}
	}
	return resourceMap, errs
}
