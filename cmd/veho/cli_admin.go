package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// nameArg is a resource's name as the command's argument.
type nameArg struct {
	Args struct {
		Name string `positional-arg-name:"name" required:"true"`
	} `positional-args:"true"`
}

// idArg is a resource's ID as the command's argument.
type idArg struct {
	Args struct {
		ID string `positional-arg-name:"id" required:"true"`
	} `positional-args:"true"`
}

// PolicyCommand is "veho policy".
type PolicyCommand struct {
	Create   PolicyCreateCommand   `command:"create" description:"Create a policy, or its next version"`
	Show     PolicyShowCommand     `command:"show" description:"Show a policy"`
	List     PolicyListCommand     `command:"list" description:"List the policies"`
	Delete   PolicyDeleteCommand   `command:"delete" description:"Delete a policy"`
	Validate PolicyValidateCommand `command:"validate" description:"Check a policy document"`
	Grant    PolicyGrantCommand    `command:"grant" description:"Grant a permission on a policy: veho policy grant <policy> <grantee> <permission>"`
	Revoke   PolicyRevokeCommand   `command:"revoke" description:"Revoke a permission on a policy"`
}

// PolicyCreateCommand is "veho policy create".
type PolicyCreateCommand struct {
	apiOptions
	nameArg
	File string `long:"file" required:"true" description:"The policy document (- for standard input)"`
}

// Execute creates the policy.
func (cmd *PolicyCreateCommand) Execute([]string) error {
	st, err := structFile(cmd.File)
	if err != nil {
		return err
	}
	return cmd.show("veho.v1.PolicyService.CreatePolicy", &vehov1.CreatePolicyRequest{Name: cmd.Args.Name, Policy: st}, &vehov1.Policy{})
}

// PolicyShowCommand is "veho policy show".
type PolicyShowCommand struct {
	apiOptions
	nameArg
	Version int32 `long:"version" description:"A version (default the latest)"`
}

// Execute prints the policy.
func (cmd *PolicyShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.PolicyService.GetPolicy", &vehov1.GetPolicyRequest{Name: cmd.Args.Name, Version: cmd.Version}, &vehov1.Policy{})
}

// PolicyListCommand is "veho policy list".
type PolicyListCommand struct{ apiOptions }

// Execute prints the policies.
func (cmd *PolicyListCommand) Execute([]string) error {
	return cmd.show("veho.v1.PolicyService.ListPolicies", &vehov1.ListPoliciesRequest{}, &vehov1.ListPoliciesResponse{}, "name", "version", "created_at")
}

// PolicyDeleteCommand is "veho policy delete".
type PolicyDeleteCommand struct {
	apiOptions
	nameArg
}

// Execute deletes the policy.
func (cmd *PolicyDeleteCommand) Execute([]string) error {
	return cmd.call("veho.v1.PolicyService.DeletePolicy", &vehov1.PolicyName{Name: cmd.Args.Name}, &vehov1.Empty{})
}

// PolicyValidateCommand is "veho policy validate".
type PolicyValidateCommand struct {
	apiOptions
	File string `long:"file" required:"true" description:"The policy document (- for standard input)"`
}

// Execute checks the document; an invalid one exits 1.
func (cmd *PolicyValidateCommand) Execute([]string) error {
	st, err := structFile(cmd.File)
	if err != nil {
		return err
	}
	resp := &vehov1.ValidatePolicyResponse{}
	if err := cmd.show("veho.v1.PolicyService.ValidatePolicy", &vehov1.ValidatePolicyRequest{Policy: st}, resp); err != nil {
		return err
	}
	if !resp.GetValid() {
		return &exitError{code: 1, msg: resp.GetCode() + ": " + resp.GetMessage()}
	}
	return nil
}

// grantArgs are a policy, a grantee and a permission.
type grantArgs struct {
	Args struct {
		Policy     string `positional-arg-name:"policy" required:"true"`
		Grantee    string `positional-arg-name:"grantee" required:"true"`
		Permission string `positional-arg-name:"permission" required:"true"`
	} `positional-args:"true"`
}

// PolicyGrantCommand is "veho policy grant".
type PolicyGrantCommand struct {
	apiOptions
	grantArgs
}

// Execute adds the grant.
func (cmd *PolicyGrantCommand) Execute([]string) error { return cmd.change(&cmd.apiOptions, true) }

// PolicyRevokeCommand is "veho policy revoke".
type PolicyRevokeCommand struct {
	apiOptions
	grantArgs
}

// Execute removes the grant.
func (cmd *PolicyRevokeCommand) Execute([]string) error { return cmd.change(&cmd.apiOptions, false) }

// change reads the policy's grants, adds or removes one, and sets them.
func (g grantArgs) change(o *apiOptions, add bool) error {
	cur := &vehov1.Grants{}
	if err := o.call("veho.v1.PolicyService.GetGrants", &vehov1.PolicyName{Name: g.Args.Policy}, cur); err != nil {
		return err
	}
	var next []*vehov1.Grant
	for _, x := range cur.GetGrants() {
		if x.GetGrantee() != g.Args.Grantee || x.GetPermission() != g.Args.Permission {
			next = append(next, x)
		}
	}
	if add {
		next = append(next, &vehov1.Grant{Grantee: g.Args.Grantee, Permission: g.Args.Permission})
	} else if len(next) == len(cur.GetGrants()) {
		return fmt.Errorf("%s has no grant of %s to %s", g.Args.Policy, g.Args.Permission, g.Args.Grantee)
	}
	return o.show("veho.v1.PolicyService.SetGrants", &vehov1.SetGrantsRequest{Name: g.Args.Policy, Grants: next}, &vehov1.Grants{}, "grantee", "permission")
}

// EndpointCommand is "veho endpoint".
type EndpointCommand struct {
	Create EndpointCreateCommand `command:"create" description:"Create an endpoint"`
	Show   EndpointShowCommand   `command:"show" description:"Show an endpoint"`
	List   EndpointListCommand   `command:"list" description:"List the endpoints"`
	Update EndpointUpdateCommand `command:"update" description:"Change an endpoint's location, configuration or credential reference"`
	Delete EndpointDeleteCommand `command:"delete" description:"Delete an endpoint"`
	Test   EndpointTestCommand   `command:"test" description:"Probe an endpoint with its credentials"`
}

// endpointFlags are the parts of an endpoint a command sets.
type endpointFlags struct {
	Location      string `long:"location" description:"A location of the topology"`
	ConfigFile    string `long:"config-file" description:"The driver's configuration, a JSON object (- for standard input)"`
	CredentialRef string `long:"credential-ref" description:"The secret of the credentials: db://<name>"`
}

// EndpointCreateCommand is "veho endpoint create".
type EndpointCreateCommand struct {
	apiOptions
	nameArg
	endpointFlags
	Driver string `long:"driver" required:"true" description:"The driver: file, s3, ..."`
}

// Execute creates the endpoint.
func (cmd *EndpointCreateCommand) Execute([]string) error {
	ep := &vehov1.Endpoint{Name: cmd.Args.Name, Driver: cmd.Driver, Location: cmd.Location, CredentialRef: cmd.CredentialRef}
	if cmd.ConfigFile != "" {
		st, err := structFile(cmd.ConfigFile)
		if err != nil {
			return err
		}
		ep.Config = st
	}
	return cmd.show("veho.v1.EndpointService.CreateEndpoint", ep, &vehov1.Endpoint{})
}

// EndpointShowCommand is "veho endpoint show".
type EndpointShowCommand struct {
	apiOptions
	nameArg
}

// Execute prints the endpoint.
func (cmd *EndpointShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.EndpointService.GetEndpoint", &vehov1.EndpointName{Name: cmd.Args.Name}, &vehov1.Endpoint{})
}

// EndpointListCommand is "veho endpoint list".
type EndpointListCommand struct{ apiOptions }

// Execute prints the endpoints.
func (cmd *EndpointListCommand) Execute([]string) error {
	return cmd.show("veho.v1.EndpointService.ListEndpoints", &vehov1.ListEndpointsRequest{}, &vehov1.ListEndpointsResponse{},
		"name", "driver", "location", "credential_ref")
}

// EndpointUpdateCommand is "veho endpoint update": what is not given stays.
type EndpointUpdateCommand struct {
	apiOptions
	nameArg
	endpointFlags
}

// Execute changes the endpoint.
func (cmd *EndpointUpdateCommand) Execute([]string) error {
	ep := &vehov1.Endpoint{}
	if err := cmd.call("veho.v1.EndpointService.GetEndpoint", &vehov1.EndpointName{Name: cmd.Args.Name}, ep); err != nil {
		return err
	}
	if cmd.Location != "" {
		ep.Location = cmd.Location
	}
	if cmd.CredentialRef != "" {
		ep.CredentialRef = cmd.CredentialRef
	}
	if cmd.ConfigFile != "" {
		st, err := structFile(cmd.ConfigFile)
		if err != nil {
			return err
		}
		ep.Config = st
	}
	ep.Driver, ep.CreatedAt = "", nil
	return cmd.show("veho.v1.EndpointService.UpdateEndpoint", ep, &vehov1.Endpoint{})
}

// EndpointDeleteCommand is "veho endpoint delete".
type EndpointDeleteCommand struct {
	apiOptions
	nameArg
}

// Execute deletes the endpoint.
func (cmd *EndpointDeleteCommand) Execute([]string) error {
	return cmd.call("veho.v1.EndpointService.DeleteEndpoint", &vehov1.EndpointName{Name: cmd.Args.Name}, &vehov1.Empty{})
}

// EndpointTestCommand is "veho endpoint test"; a failed probe exits 1.
type EndpointTestCommand struct {
	apiOptions
	nameArg
}

// Execute probes the endpoint.
func (cmd *EndpointTestCommand) Execute([]string) error {
	resp := &vehov1.EndpointTest{}
	if err := cmd.show("veho.v1.EndpointService.TestEndpoint", &vehov1.EndpointName{Name: cmd.Args.Name}, resp); err != nil {
		return err
	}
	if !resp.GetOk() {
		return &exitError{code: 1, msg: fmt.Sprintf("the %s step failed: %s: %s", resp.GetStep(), resp.GetCode(), resp.GetMessage())}
	}
	return nil
}

// SecretCommand is "veho secret".
type SecretCommand struct {
	Set    SecretSetCommand    `command:"set" description:"Create or replace a secret; the value from a file or standard input only"`
	Show   SecretShowCommand   `command:"show" description:"Show a secret's metadata (never its value)"`
	List   SecretListCommand   `command:"list" description:"List the secrets"`
	Delete SecretDeleteCommand `command:"delete" description:"Delete a secret"`
}

// SecretSetCommand is "veho secret set".
type SecretSetCommand struct {
	apiOptions
	nameArg
	FromFile string `long:"from-file" description:"The value's file"`
	Stdin    bool   `long:"stdin" description:"Read the value from standard input"`
}

// Execute writes the secret (C8-1: never from an argument).
func (cmd *SecretSetCommand) Execute([]string) error {
	var (
		value []byte
		err   error
	)
	switch {
	case (cmd.FromFile == "") == !cmd.Stdin:
		return errors.New("give --from-file or --stdin")
	case cmd.Stdin:
		value, err = io.ReadAll(os.Stdin)
	default:
		value, err = os.ReadFile(cmd.FromFile)
	}
	if err != nil {
		return err
	}
	defer clear(value)
	return cmd.show("veho.v1.SecretService.PutSecret", &vehov1.PutSecretRequest{Name: cmd.Args.Name, Value: value}, &vehov1.Secret{})
}

// SecretShowCommand is "veho secret show".
type SecretShowCommand struct {
	apiOptions
	nameArg
}

// Execute prints the secret's metadata.
func (cmd *SecretShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.SecretService.GetSecret", &vehov1.SecretName{Name: cmd.Args.Name}, &vehov1.Secret{})
}

// SecretListCommand is "veho secret list".
type SecretListCommand struct{ apiOptions }

// Execute prints the secrets.
func (cmd *SecretListCommand) Execute([]string) error {
	return cmd.show("veho.v1.SecretService.ListSecrets", &vehov1.ListSecretsRequest{}, &vehov1.ListSecretsResponse{}, "name", "version", "updated_at")
}

// SecretDeleteCommand is "veho secret delete".
type SecretDeleteCommand struct {
	apiOptions
	nameArg
}

// Execute deletes the secret.
func (cmd *SecretDeleteCommand) Execute([]string) error {
	return cmd.call("veho.v1.SecretService.DeleteSecret", &vehov1.SecretName{Name: cmd.Args.Name}, &vehov1.Empty{})
}

// APIKeyCommand is "veho apikey".
type APIKeyCommand struct {
	Create APIKeyCreateCommand `command:"create" description:"Create a principal and its API key; the key is printed once"`
	List   APIKeyListCommand   `command:"list" description:"List the tenant's API keys (never their secrets)"`
	Revoke APIKeyRevokeCommand `command:"revoke" description:"Revoke an API key at once"`
}

// APIKeyCreateCommand is "veho apikey create".
type APIKeyCreateCommand struct {
	apiOptions
	Name    string `long:"name" required:"true" description:"The principal's name"`
	Role    string `long:"role" required:"true" choice:"viewer" choice:"operator" choice:"admin" description:"Its role"`
	Expires int64  `long:"expires-in" description:"Seconds until it expires; 0 never"`
}

// Execute creates the key and prints it, once.
func (cmd *APIKeyCreateCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.CreateKey", &vehov1.CreateKeyRequest{Name: cmd.Name, Role: cmd.Role, ExpiresIn: cmd.Expires}, &vehov1.CreatedKey{})
}

// APIKeyListCommand is "veho apikey list".
type APIKeyListCommand struct{ apiOptions }

// Execute prints the keys.
func (cmd *APIKeyListCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.ListKeys", &vehov1.ListKeysRequest{}, &vehov1.ListKeysResponse{},
		"id", "principal", "role", "expires_at", "revoked_at", "last_used_at")
}

// APIKeyRevokeCommand is "veho apikey revoke".
type APIKeyRevokeCommand struct {
	apiOptions
	idArg
}

// Execute revokes the key.
func (cmd *APIKeyRevokeCommand) Execute([]string) error {
	return cmd.call("veho.v1.AdminService.RevokeKey", &vehov1.KeyID{Id: cmd.Args.ID}, &vehov1.Empty{})
}

// EntitlementCommand is "veho entitlement".
type EntitlementCommand struct {
	Create EntitlementCreateCommand `command:"create" description:"Entitle an OIDC or SPIFFE identity, or a group, to a role in the tenant"`
	List   EntitlementListCommand   `command:"list" description:"List the tenant's entitlements"`
	Delete EntitlementDeleteCommand `command:"delete" description:"Delete an entitlement"`
}

// EntitlementCreateCommand is "veho entitlement create".
type EntitlementCreateCommand struct {
	apiOptions
	Kind    string `long:"kind" required:"true" choice:"oidc" choice:"spiffe" description:"The kind of identity"`
	Issuer  string `long:"issuer" required:"true" description:"The OIDC issuer URL, or the SPIFFE trust domain"`
	Subject string `long:"subject" description:"One identity: the OIDC sub, or the SPIFFE ID"`
	Group   string `long:"group" description:"The members of an OIDC group"`
	Role    string `long:"role" required:"true" choice:"viewer" choice:"operator" choice:"admin" description:"The role"`
}

// Execute creates the entitlement.
func (cmd *EntitlementCreateCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.CreateEntitlement", &vehov1.Entitlement{Kind: cmd.Kind, Issuer: cmd.Issuer, Subject: cmd.Subject,
		Group: cmd.Group, Role: cmd.Role}, &vehov1.Entitlement{})
}

// EntitlementListCommand is "veho entitlement list".
type EntitlementListCommand struct{ apiOptions }

// Execute prints the entitlements.
func (cmd *EntitlementListCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.ListEntitlements", &vehov1.ListEntitlementsRequest{}, &vehov1.ListEntitlementsResponse{},
		"id", "kind", "issuer", "subject", "group", "role")
}

// EntitlementDeleteCommand is "veho entitlement delete".
type EntitlementDeleteCommand struct {
	apiOptions
	idArg
}

// Execute deletes the entitlement.
func (cmd *EntitlementDeleteCommand) Execute([]string) error {
	return cmd.call("veho.v1.AdminService.DeleteEntitlement", &vehov1.EntitlementID{Id: cmd.Args.ID}, &vehov1.Empty{})
}

// SettingsCommand is "veho settings".
type SettingsCommand struct {
	Show   SettingsShowCommand   `command:"show" description:"Show the tenant's settings"`
	Update SettingsUpdateCommand `command:"update" description:"Change the tenant's settings"`
}

// SettingsShowCommand is "veho settings show".
type SettingsShowCommand struct{ apiOptions }

// Execute prints the settings.
func (cmd *SettingsShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.GetSettings", &vehov1.GetSettingsRequest{}, &vehov1.TenantSettings{})
}

// SettingsUpdateCommand is "veho settings update".
type SettingsUpdateCommand struct {
	apiOptions
	InlinePolicies string `long:"inline-policies" required:"true" choice:"all" choice:"granted" choice:"none" description:"Who may submit inline policies"`
}

// Execute changes the settings.
func (cmd *SettingsUpdateCommand) Execute([]string) error {
	return cmd.show("veho.v1.AdminService.UpdateSettings", &vehov1.TenantSettings{InlinePolicies: cmd.InlinePolicies}, &vehov1.TenantSettings{})
}

// TenantCommand is "veho tenant".
type TenantCommand struct {
	Create TenantCreateCommand `command:"create" description:"Create a tenant"`
	Show   TenantShowCommand   `command:"show" description:"Show a tenant"`
	List   TenantListCommand   `command:"list" description:"List the tenants"`
	Update TenantUpdateCommand `command:"update" description:"Suspend or reactivate a tenant, or set its quotas"`
}

// TenantCreateCommand is "veho tenant create".
type TenantCreateCommand struct {
	apiOptions
	nameArg
}

// Execute creates the tenant.
func (cmd *TenantCreateCommand) Execute([]string) error {
	return cmd.show("veho.v1.TenantService.CreateTenant", &vehov1.CreateTenantRequest{Name: cmd.Args.Name}, &vehov1.Tenant{})
}

// TenantShowCommand is "veho tenant show".
type TenantShowCommand struct {
	apiOptions
	idArg
}

// Execute prints the tenant.
func (cmd *TenantShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.TenantService.GetTenant", &vehov1.TenantID{Id: cmd.Args.ID}, &vehov1.Tenant{})
}

// TenantListCommand is "veho tenant list".
type TenantListCommand struct{ apiOptions }

// Execute prints the tenants.
func (cmd *TenantListCommand) Execute([]string) error {
	return cmd.show("veho.v1.TenantService.ListTenants", &vehov1.ListTenantsRequest{}, &vehov1.ListTenantsResponse{}, "id", "name", "state", "quotas")
}

// TenantUpdateCommand is "veho tenant update".
type TenantUpdateCommand struct {
	apiOptions
	idArg
	State            string        `long:"state" choice:"active" choice:"suspended" description:"The tenant's state"`
	MaxDestinations  int32         `long:"max-destinations" description:"The most destinations of a job"`
	MaxActiveJobs    int32         `long:"max-active-jobs" description:"The most jobs in progress"`
	MinRetryInitial  time.Duration `long:"min-retry-initial" description:"The shortest retry.initial a policy may set (at most 2s; default 1s)"`
	MaxRetryInterval time.Duration `long:"max-retry-interval" description:"The longest retry.max_interval a policy may set (at least 15m; default 1h)"`
}

// Execute changes the tenant.
func (cmd *TenantUpdateCommand) Execute([]string) error {
	req := &vehov1.UpdateTenantRequest{Id: cmd.Args.ID, State: cmd.State}
	if cmd.MaxDestinations != 0 || cmd.MaxActiveJobs != 0 || cmd.MinRetryInitial != 0 || cmd.MaxRetryInterval != 0 {
		req.Quotas = &vehov1.Quotas{MaxDestinations: cmd.MaxDestinations, MaxActiveJobs: cmd.MaxActiveJobs}
		if cmd.MinRetryInitial != 0 {
			req.Quotas.MinRetryInitial = durationpb.New(cmd.MinRetryInitial)
		}
		if cmd.MaxRetryInterval != 0 {
			req.Quotas.MaxRetryInterval = durationpb.New(cmd.MaxRetryInterval)
		}
	}
	return cmd.show("veho.v1.TenantService.UpdateTenant", req, &vehov1.Tenant{})
}
