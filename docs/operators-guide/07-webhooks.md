# Webhooks

Webhooks tell a client that a job has settled, without the client polling. They are off by default: polling and long polling work without them, and a client that misses a webhook finds the same state by asking.

## Enabling

On every node, in `veho.yaml`:

```yaml
notifications:
  enabled: true
  allow_cidrs: []          # private ranges a webhook may reach, e.g. 10.20.0.0/16
```

Until `notifications.enabled` is true, a tenant's request to register a webhook is refused with `invalid_state`, and no event is written. veho never delivers to loopback, private, link-local, multicast or cloud-metadata addresses: where a tenant's receiver lives in a private network, list that network in `allow_cidrs` (the metadata addresses can never be listed). The list applies to every tenant of the cluster.

Deliveries leave from whichever node holds the dispatcher lock, so every node must be able to reach the receivers, and the firewall must allow it.

## What a tenant does

A tenant administrator manages webhooks with `veho webhook`:

```bash
veho webhook create ci --url https://hooks.example.com/veho --event job.succeeded --event job.failed
veho webhook list
veho webhook test ci                    # a signed ping, answered at once; exit 1 if it fails
veho webhook update ci --disable        # or --enable, or --event ...
veho webhook rotate ci                  # a new secret, shown once; the old one signs for 24 hours too
veho webhook delete ci
```

`create` and `rotate` print the secret once; it is kept only, encrypted, in the secret store, under a name that starts with `webhook.`, which the tenant's secrets API neither lists nor lets it write. A tenant has at most 10 webhooks.

## What a receiver gets

A `POST` of at most 4 KiB, `Content-Type: application/json`, with `Veho-Event-Id`, `Veho-Event-Type` and `Veho-Signature: t=<unix seconds>,v1=<hex>`. The body carries the event's `id`, `type`, `occurred_at`, the `tenant_id` and the job's ID, state, settlement time and the number of destinations that succeeded and failed; no object names, paths or error text. A `2xx` within 10 seconds is a success; anything else, a redirect included, is a failure.

A receiver verifies, before it trusts anything:

1. compute `HMAC-SHA256(secret, t + "." + body)` in hexadecimal, and compare it in constant time with each `v1` value (there are two during a rotation);
2. refuse a `t` more than five minutes from its own clock;
3. deduplicate on `Veho-Event-Id`: delivery is at least once, and a retry carries the same ID. Order is not promised.

```bash
printf '%s.%s' "$T" "$BODY" | openssl dgst -sha256 -hmac "$SECRET"
```

## Delivery, retries and what to watch

An event waits 30 seconds after a failed delivery, then 1, 2, 4 minutes, and so on up to an hour between attempts; after 12 attempts it is **abandoned** and audited (`WEBHOOK_DELIVERY_ABANDONED`). After 12 consecutive failed deliveries the webhook is switched off (`WEBHOOK_DISABLE`, with the reason in `veho webhook show`); the tenant switches it on again with `veho webhook update --enable`, once the receiver is fixed. A stuck webhook delays only its own events.

Delivered events are kept `notifications.delivered_retention` (24 hours), abandoned and undelivered ones `notifications.abandoned_retention` (30 days). A webhook keeps at most 10,000 pending events: beyond that the oldest are abandoned.

| Watch | Meaning |
| :------------------------------------------------ | :--------------------------------------------------------- |
| `veho_webhook_outbox_pending` rising for an hour | Receivers are down, or no node can reach them, or the dispatcher is stuck |
| `veho_webhook_deliveries_total{result="abandoned"}` | Events given up: tell the tenant; their jobs are unaffected |
| `WEBHOOK_DISABLE` in the audit log | A receiver failed for about a day |

Webhooks never affect a job: the event is written in the transaction that settles it, and delivery happens afterwards, on its own.
