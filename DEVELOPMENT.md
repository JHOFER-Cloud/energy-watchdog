# Development

## Running the whole thing locally

There is no mocking layer. `cmd/fakecluster` serves just enough Proxmox and Prometheus API
that the real binary runs against it unchanged — same reconcile loop, same Proxmox client,
same UI, no hardware and no dry-run special-casing. This is the same approach the tests take
(`internal/controller/reconcile_test.go` stands up `httptest` servers speaking fake Proxmox
JSON); `fakecluster` is just that, long-lived and with knobs.

```sh
go run ./cmd/fakecluster -addr 127.0.0.1:8006 -up=true -surplus=-300 &
go run . -config config.local.yaml -dev-user josef -dev-groups deskvm-josef,jhc-admins
```

Then open <http://localhost:8080>. Metrics are on <http://localhost:9333/metrics>.

`config.local.yaml` points everything at the fake and uses a 5s interval so you aren't waiting
a minute per tick.

### Driving it

```sh
curl 'localhost:8006/fake/surplus?w=-500'   # deficit: watchdog sheds p1
curl 'localhost:8006/fake/surplus?w=2500'   # surplus: watchdog wakes it again
curl localhost:8006/fake/state              # node/guest/surplus as the fake sees it
```

The fake takes `bootDelay` (6s) to complete a power-off or a wake, so the UI's
"Waking the server…" and "Server was shutting down…" states are actually on screen long
enough to look at. Guest start/stop is instant.

Preloaded guests: `101` (migrate class), `301` (stop class), `601`/`602` (gaming guard /
desktop VMs).

### -dev-user

`-dev-user` **skips SSO entirely** and treats every request as that user. It exists so the UI
can be developed without authentik, and it logs a warning at startup. Without the flag the
server verifies real authentik tokens and there is no way to reach the handlers otherwise —
see below.

To test authorisation logic, change the groups: `-dev-groups deskvm-josef` gives a non-admin
who can start 601 but not toggle the shed; passing no groups gives someone who sees no VMs.

## How the pieces fit

The invariant that shapes everything: **the reconcile loop is the only thing that touches
p1's power state.** The deployment runs one replica with `strategy: Recreate` for the same
reason.

So the UI does not wake p1 or start VMs itself. It records *intent*, and the loop acts on it:

```
browser ──POST /api/vms/601/start──> API ──writes intent.json──> ConfigMap
                                      │
                                      └──Nudge()──> reconcile loop ──> Proxmox
```

`Nudge()` (`internal/controller/controller.go`) makes the loop reconcile immediately instead
of at the next tick, so a click feels instant without the API becoming a second writer.

### state vs intent

The state ConfigMap has two keys, written by different parties:

| Key | Written by | Contains |
|---|---|---|
| `state.json` | the reconcile loop | mode, stopped guests, grace clock (status) |
| `intent.json` | the API, or you with kubectl | manual shed, wake requests (spec) |

They are written by **separate merge patches**, never a whole-object PUT. That matters: a
migrate can run 15 minutes, and a state save landing at the end of it must not wipe an intent
set while it was in flight. `TestConfigMapStoreKeysAreIndependent` pins this down.

The loop never writes intent. Wake requests therefore aren't cleared when satisfied — they
age out after `state.WakeRequestTTL` (10m). By then either the VM is up, and the *gaming
guard* is what holds p1, or it failed and retrying is the user's call.

### Manual shed

`Decide` treats it as a permanent deficit:

```go
if s.ManualShed {
    sig = sigDeficit
}
```

That one line is the whole feature. Every existing rule then applies unchanged — gaming guard
still vetoes the power-off, the grace window still runs, powering p1 on by hand is still
adopted — and `sigSurplus` becomes unreachable, so solar can't wake it.

**`energy_watchdog_mode` deliberately keeps reporting `shed`, not `manual`.** nut-dog's wake
inhibit queries `energy_watchdog_mode{mode="shed"} == 1`
(`fleet/infra/nut-dog/common/config.yaml`); a new mode value would silently switch that off
and nut-dog would wake p1 mid-shed. The "why" lives in the separate
`energy_watchdog_manual_shed` gauge.

### Break-glass

If the UI is down, intent is just a ConfigMap key:

```sh
kubectl -n energy patch cm energy-watchdog-state --type merge \
  -p '{"data":{"intent.json":"{\"shed\":true}"}}'
```

The loop picks it up on the next tick. Set `false` to hand control back to the sun.

## Authentication

The API trusts nothing it isn't given a signature for.

Authentik's forward-auth middleware (`fleet-auth`, `default-traefik-middleware.yaml`) injects
`X-authentik-username` and `X-authentik-groups`, but **those are ignored** — anything able to
reach the pod could set them. Identity comes only from the signed `X-authentik-Jwt`, verified
with go-oidc against authentik's published keys.

**The signing keys are fetched from the issuer's discovery document, never from the request.**
The middleware also forwards `X-authentik-meta-jwks`, and verifying a token against a JWKS
taken from the same request is worthless — a forger supplies both. `selfService.issuerURL` is
the trust anchor.

OIDC discovery runs in the background with backoff, not at startup, so authentik being down
degrades the API to 503 while the reconcile loop keeps shedding and waking p1 normally.

## Testing

```sh
go test ./...
```

The decision tests are the interesting ones — `Decide` is pure, so the whole state machine is
covered without hardware: `decide_test.go` for the solar path, `manual_test.go` for manual
shed and wake requests.

When adding a test that asserts something *doesn't* happen, check it actually bites by
breaking the code and watching it fail. An auth test that passes because the request never
reached the handler is worse than no test.
