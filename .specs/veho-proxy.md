# veho: drivers through a forward proxy

This file specifies how a driver reaches a storage service through an HTTP forward proxy: the `s3` driver first, so that a backup from an on-premise source to AWS S3 works where every non-corporate connection must go through a corporate proxy. It is part of `veho_specification.md`; it extends the dial guard of `veho-sources.md` §4 (SRC-3, SEC-13) and the network checks of `veho-network-checks.md`.

Status: approved, 2026-11-25, for the `s3`, `http` and `nexus` drivers together (decision 7); §11 records what the implementation changed.

## 1. Why

veho dials every storage service itself, through the dial guard, and uses no proxy (`veho-sources.md` §4): through a proxy the guard would see the proxy's address, not the target's, and a tenant's endpoint could make the proxy reach the metadata service or an internal host. But a common deployment has an on-premise network whose hosts reach the Internet only through a proxy, and **resolve only corporate names**: the proxy alone resolves the external ones. A backup from an on-premise S3 (or SFTP) to AWS S3 is then impossible.

## 2. Decisions (approved 2026-11-25)

| # | Question | Decision |
| :--- | :--- | :--- |
| 1 | Who chooses the proxy | The operator, per driver, in `drivers[].settings.proxy`. Never a tenant, never the environment (`HTTPS_PROXY` stays ignored): the configuration is the one source, and it is visible. |
| 2 | Which targets go through it | Those whose **host name** matches `proxy.hosts`, a list of exact names (`s3.backup.example.net`) and suffix patterns (`*.amazonaws.com`: any name ending in `.amazonaws.com`). The match is on the name, case-insensitive, after the name is normalised (no trailing dot); veho does **not** resolve it: only the proxy does. |
| 3 | What never goes through it | An IP literal (`169.254.169.254`, `10.1.2.3`, `[fd00::1]`), a name not in `proxy.hosts`, and plain `http`. They are dialled directly, through the guard, as today (so an IP literal in a private range is reachable only through `allow_cidrs`, and the metadata addresses never). |
| 4 | How | `https` through `CONNECT host:port` to the proxy, then TLS end to end with the target, verified for the target's name against the system roots and the endpoint's CA. The proxy sees the host name and port, never the content or the credentials. |
| 5 | The proxy's own address | Dialled directly; it is the operator's, so the guard allows it (whatever its range) and only it. Its URL is `http://` (the `CONNECT` is in clear, the tunnel is TLS) or `https://` (TLS to the proxy too, verified against `proxy.ca_cert` or the system roots). |
| 6 | Proxy authentication | Optional Basic credentials, from a file the operator names (`proxy.credentials_file`, `user:password`, mode 0600), sent only to the proxy in `Proxy-Authorization`; never in the configuration's text, never in a log. |
| 7 | Which drivers | `s3`, `http` and `nexus`, the drivers built on `net/http`, through one shared dialer (`internal/netguard`). |
| 8 | Network checks | A proxied flow is probed as the path a transfer takes: TCP (or TLS) to the proxy, `CONNECT` to the target, then the TLS handshake with the target's name. Its result says `via proxy`. |

## 3. Settings

```yaml
drivers:
  - name: s3
    settings:
      allow_cidrs: [10.20.0.0/16]          # direct, through the guard: the on-premise S3
      proxy:
        url: http://proxy.corp.example:3128
        hosts: ["*.amazonaws.com", "s3.backup.example.net"]
        credentials_file: /etc/veho/proxy-credentials   # optional, user:password
        ca_cert_file: /etc/veho/proxy-ca.pem            # optional, for an https proxy
```

- `proxy.url` is required with `proxy`; `hosts` must not be empty; a pattern is `*.` followed by at least two labels (`*.com` is refused), or an exact name; neither may be an IP address.
- The settings are checked when the driver is built: a node with a bad `proxy` does not start.

## 4. The dialer

`internal/netguard` gains a proxy-aware transport for the drivers built on `net/http`:

```go
// Route is the dial plan of one request: direct through the guard, or a
// CONNECT tunnel through the proxy.
func (p *Proxy) Transport(g *Guard, tlsConfig *tls.Config) *http.Transport
```

- `Transport.Proxy` is a function of the request: the proxy's URL when the request is `https` and its host matches `hosts` (decision 2), else `nil` (direct).
- `Transport.DialContext` is the guard's dialer, with one exception: the proxy's address is allowed (decision 5). Go's transport dials the proxy through it, sends `CONNECT`, and runs TLS with the target's name inside the tunnel.
- A request whose host is in `hosts` but whose scheme is `http` is refused, `Permanent`: "plain http is not sent through the proxy".
- No redirect is followed (SEC-13), so a request never moves from a proxied name to another.
- A `CONNECT` the proxy refuses (`403`, `407`, `502`) is `Permanent` for `403` and `407` (a policy or credential problem of the operator), `Transient` for `502`, `503`, `504`.

## 5. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| PX-1 | **The proxy as SSRF relay**: a tenant's endpoint names an internal host or the metadata service, and the proxy, which can reach them, connects. | Only names in the operator's `hosts` go through the proxy; IP literals never do (decision 3); everything else is dialled directly through the guard. |
| PX-2 | **A listed name resolves, at the proxy, to an internal address** (split DNS, a wildcard over a domain the tenant controls, a rebinding). | The operator lists provider names or exact names, never a domain a tenant controls (the Operator's Guide says so); TLS is verified for the name, so only a host with a valid certificate for it answers; the proxy's own policy should refuse internal destinations. Residual: R-20. |
| PX-3 | **Credentials or content seen by the proxy.** | `CONNECT` only, TLS end to end; plain `http` is never proxied; the S3 request's signature is inside the tunnel. |
| PX-4 | **The proxy's credentials leak.** | Read from a 0600 file the operator names, sent only to the proxy, never logged nor shown in a check's results. |
| PX-5 | **The environment's proxy settings redirect veho's traffic.** | `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` are ignored by every driver. |
| PX-6 | **A tenant learns which names the proxy reaches**, through the checks. | A check probes only the endpoint's own flows; its result says `via proxy` and whether it was reached, not the proxy's address or list. |

New residual risk:

- **R-20** A name the operator lists in `proxy.hosts` may resolve, at the proxy, to an address veho would refuse, because veho does not resolve proxied names (the node may not be able to). Mitigated by the operator's list, by TLS verification for the name and by the proxy's own policy. Accepted.

## 6. Network checks

`veho-network-checks.md` §3 gains `Flow.Proxy` (the proxy's `host:port`, empty when direct), set by the driver from its settings for a flow whose host is in `hosts`. The probe (`internal/flowcheck`) then:

- does not resolve the target;
- checks the proxy's address with the guard as decision 5 allows;
- runs a netcheck `tls` check to the target's name whose connection is the proxy's (its `AfterConnect` hook sends `CONNECT host:port` with the credentials, and expects `200`), so the handshake verifies the target through the tunnel;
- reports `Address` as the proxy's `host:port`, `Protocol` as `tls via proxy`.

## 7. Lab

A proxy node in the veho lab (`lab.sh proxy up|down`, optional like the Nexus node): a Squid container on the lab's WAN side, with its own DNS (dnsmasq) that alone resolves `s3.outside.veho.lab`, a Garage container reachable only from the proxy, with a certificate of the lab CA for that name. The veho nodes resolve only the lab's names and have no route to it.

## 8. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| PX-T1 | The settings: a pattern too broad, an IP in `hosts`, no `url`, a credentials file that is not 0600, refused | unit |
| PX-T2 | The route: a listed name over `https` proxied without a resolution; an unlisted name, an IP literal, the metadata address, direct through the guard; a listed name over `http` refused; a redirect not followed | unit |
| PX-T3 | Through a fake proxy (`CONNECT` handled by the test): the TLS name verified through the tunnel, `407` and `403` Permanent, `502` Transient, the credentials sent to the proxy only | unit |
| PX-T4 | The probe of a proxied flow through the fake proxy, reachable and unreachable; the result without the proxy's credentials | unit |
| PX-T5 | In the lab: a job from region a's Garage (direct) to the outside Garage (through Squid), and back; `endpoint check` of the outside endpoint from the six nodes, `via proxy`; an endpoint at the outside Garage's IP refused without the proxy seeing a request | lab |

## 9. Documentation

The Setup Guide (the `proxy` settings, the credentials file), the Operator's Guide (when a proxy is needed, how to write `hosts`, PX-2 and R-20, the proxy's own policy), the Architecture and Design Guide (the dialer, the threat rows), `veho-threat-model.md` (T-04 extended, R-20), `veho-sources.md` §4 (the proxy as the one exception to "no proxy").

## 10. Out of scope

SOCKS proxies; PAC files; a proxy for the webhooks; the DevStack drivers (Keystone's catalog names are the cloud's, so a proxy for them needs its own list).

## 11. Changes from the implementation (2026-11-25)

- **`http://` proxies only.** The `CONNECT` is in clear and the tunnel is TLS with the target, which is what a corporate proxy offers; a TLS connection to the proxy itself would gain little (the target's TLS protects the content and the credentials) and the probes could not follow it. `proxy.ca_cert_file` is dropped, and an `https://` proxy URL is refused when the driver is built.
- **The route is decided by one shared `netguard.Proxy`** (`Routes(scheme, host)`), which the drivers and the probes build from the same `drivers[].settings.proxy`: the probes need no `Flow.Proxy` field (§6), the engine builds the proxy of the endpoint's driver as it builds its guard.
- A `CONNECT` refused is reported through `http.Transport.OnProxyConnectResponse` as a `netguard.ProxyError` with the proxy's status, which each driver classifies (decision §4).
- **The lab** (`test/lab/lib/proxy.sh`, `lab.sh proxy up|down|requests`): `proxy-a` (Squid, `CONNECT` to 443 from the regions only, and a dnsmasq that alone knows `s3.outside.veho.lab`) with a second interface on the bridge `outside`; `out-s3` (Garage behind nginx, the lab CA's certificate for the name) on `outside` only, which the lab VM does not forward to or from the regions (table `inet veho_outside`). The nodes get the proxy in the s3 driver's settings when `proxy-a` runs, and the lab CA in their system roots (the `s3` driver has no `ca_cert`). PX-T5 (`test/acceptance/proxy_test.go`) passes on the six nodes: a 24 MiB object to the outside S3 and back with the same SHA-256, its check `tls via proxy` reachable from every node, the S3 by its address failed with no request seen by Squid.
- Tests: PX-T1 to PX-T3 (`internal/netguard/proxy_test.go`, and `pkg/plugin/nexus/proxy_test.go` for a driver), PX-T4 (`internal/flowcheck`, `TestProxied`), PX-T5 as above.
