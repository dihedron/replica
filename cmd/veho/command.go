package main

import "errors"

// Command is the root of the command line (.specs/veho-cli.md §1): the
// process commands, the database's own (as veho_migrator), and the API's
// clients.
type Command struct {
	Migrate   MigrateCommand   `command:"migrate" description:"Apply the database migrations embedded in this binary, as veho_migrator"`
	Serve     ServeCommand     `command:"serve" description:"Run a node: the API and the workflow executor"`
	Bootstrap BootstrapCommand `command:"bootstrap" description:"Create the first tenants, keys and entitlements in the database, as veho_migrator"`
	KEK       KEKCommand       `command:"kek" description:"Manage the key-encryption keys of the secrets, as veho_migrator"`

	Job            JobCommand            `command:"job" description:"Submit, show, wait for, cancel and retry jobs"`
	Policy         PolicyCommand         `command:"policy" description:"Manage named policies and their grants"`
	Endpoint       EndpointCommand       `command:"endpoint" description:"Manage the tenant's storage endpoints"`
	Secret         SecretCommand         `command:"secret" description:"Manage the tenant's secrets (write-only)"`
	Webhook        WebhookCommand        `command:"webhook" description:"Manage the tenant's webhooks for job completion"`
	Auth           AuthCommand           `command:"auth" description:"Sign in with your identity provider (OIDC), and see who you are"`
	ServiceAccount ServiceAccountCommand `command:"serviceaccount" description:"Manage the service accounts, whose API keys rotate under their grants"`
	APIKey         APIKeyCommand         `command:"apikey" description:"Manage the service accounts' API keys"`
	Entitlement    EntitlementCommand    `command:"entitlement" description:"Manage the entitlements of OIDC and SPIFFE identities in the tenant"`
	Settings       SettingsCommand       `command:"settings" description:"Show and change the tenant's settings"`
	Tenant         TenantCommand         `command:"tenant" description:"Manage the tenants (platform administrators)"`
}

// Execute runs when no subcommand is given.
func (cmd *Command) Execute(_ []string) error {
	return errors.New("no command given: run \"veho version\" or \"veho --help\"")
}
