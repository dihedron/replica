package main

import (
	"fmt"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
)

// WebhookCommand is "veho webhook" (.specs/veho-webhooks.md §3.1).
type WebhookCommand struct {
	Create WebhookCreateCommand `command:"create" description:"Register a webhook; its secret is shown once"`
	Show   WebhookShowCommand   `command:"show" description:"Show a webhook"`
	List   WebhookListCommand   `command:"list" description:"List the webhooks"`
	Update WebhookUpdateCommand `command:"update" description:"Change a webhook's events, or switch it on or off"`
	Rotate WebhookRotateCommand `command:"rotate" description:"Give a webhook a new secret, shown once; the old one still signs for 24 hours"`
	Delete WebhookDeleteCommand `command:"delete" description:"Delete a webhook"`
	Test   WebhookTestCommand   `command:"test" description:"Post a signed ping to a webhook now"`
}

// WebhookCreateCommand is "veho webhook create".
type WebhookCreateCommand struct {
	apiOptions
	nameArg
	URL    string   `long:"url" required:"true" description:"The https URL to call"`
	Events []string `long:"event" required:"true" description:"An event to send: job.succeeded, job.failed, job.cancelled (repeat for more)"`
}

// Execute registers the webhook.
func (cmd *WebhookCreateCommand) Execute([]string) error {
	return cmd.show("veho.v1.WebhookService.CreateWebhook",
		&vehov1.CreateWebhookRequest{Name: cmd.Args.Name, Url: cmd.URL, Events: cmd.Events}, &vehov1.CreatedWebhook{})
}

// WebhookShowCommand is "veho webhook show".
type WebhookShowCommand struct {
	apiOptions
	nameArg
}

// Execute prints the webhook.
func (cmd *WebhookShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.WebhookService.GetWebhook", &vehov1.WebhookName{Name: cmd.Args.Name}, &vehov1.Webhook{})
}

// WebhookListCommand is "veho webhook list".
type WebhookListCommand struct{ apiOptions }

// Execute prints the webhooks.
func (cmd *WebhookListCommand) Execute([]string) error {
	return cmd.show("veho.v1.WebhookService.ListWebhooks", &vehov1.ListWebhooksRequest{}, &vehov1.ListWebhooksResponse{},
		"name", "url", "enabled", "consecutive_failures")
}

// WebhookUpdateCommand is "veho webhook update": what is not given stays.
type WebhookUpdateCommand struct {
	apiOptions
	nameArg
	Events  []string `long:"event" description:"Replace the events with these (repeat for more)"`
	Enable  bool     `long:"enable" description:"Switch the webhook on, and clear the failures that disabled it"`
	Disable bool     `long:"disable" description:"Switch the webhook off"`
}

// Execute changes the webhook.
func (cmd *WebhookUpdateCommand) Execute([]string) error {
	if cmd.Enable && cmd.Disable {
		return fmt.Errorf("--enable and --disable exclude each other")
	}
	req := &vehov1.UpdateWebhookRequest{Name: cmd.Args.Name, Events: cmd.Events}
	if cmd.Enable || cmd.Disable {
		on := cmd.Enable
		req.Enabled = &on
	}
	return cmd.show("veho.v1.WebhookService.UpdateWebhook", req, &vehov1.Webhook{})
}

// WebhookRotateCommand is "veho webhook rotate".
type WebhookRotateCommand struct {
	apiOptions
	nameArg
}

// Execute rotates the secret.
func (cmd *WebhookRotateCommand) Execute([]string) error {
	return cmd.show("veho.v1.WebhookService.RotateWebhookSecret", &vehov1.WebhookName{Name: cmd.Args.Name}, &vehov1.WebhookSecret{})
}

// WebhookDeleteCommand is "veho webhook delete".
type WebhookDeleteCommand struct {
	apiOptions
	nameArg
}

// Execute deletes the webhook.
func (cmd *WebhookDeleteCommand) Execute([]string) error {
	return cmd.call("veho.v1.WebhookService.DeleteWebhook", &vehov1.WebhookName{Name: cmd.Args.Name}, &vehov1.Empty{})
}

// WebhookTestCommand is "veho webhook test"; a failed ping exits 1.
type WebhookTestCommand struct {
	apiOptions
	nameArg
}

// Execute posts the ping.
func (cmd *WebhookTestCommand) Execute([]string) error {
	resp := &vehov1.WebhookTest{}
	if err := cmd.show("veho.v1.WebhookService.TestWebhook", &vehov1.WebhookName{Name: cmd.Args.Name}, resp); err != nil {
		return err
	}
	if !resp.GetOk() {
		return &exitError{code: 1, msg: fmt.Sprintf("the ping failed: %s (status %d)", resp.GetClass(), resp.GetStatus())}
	}
	return nil
}
