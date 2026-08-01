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
age out after `gamingGrace`. By then either the VM is up, and the *gaming guard* is what
holds p1, or it failed and retrying is the user's call.

The TTL is `gamingGrace` rather than a constant of its own on purpose: a request holds p1 up
for exactly as long as a gaming session gets to produce a running VM, so tuning one can't
open a window where a request outlives the grace that honours it.

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

energy-watchdog does its own OIDC login against authentik — the authorization-code flow with
PKCE — exactly like every other service in the fleet. There is no forward-auth proxy in the
request path, so nothing has to be trusted to inject identity headers.

After a successful login the app issues its own session cookie: `payload.HMAC-SHA256(payload)`,
signed with `selfService.sessionKey`. It is stateless, so a restart doesn't sign everyone out,
and it can't be edited to change the user or their groups without invalidating the signature.
Rotating `sessionKey` is how you force a global re-login.

Two things the callback checks before it will issue a session, both in `completeLogin`:

- **state** — compared against the value in the (signed) flow cookie, so a third party can't
  feed us an authorization code of their choosing.
- **expiry** — a half-finished login is only good for `flowTTL` (10m).

The redirect URI is derived from `selfService.externalURL` as `<externalURL>/auth/callback`,
so the app and the authentik provider can't drift apart. `redirect_uris` in the blueprint has
to match it exactly.

OIDC discovery runs in the background with backoff, not at startup, so authentik being down
degrades the UI to 503 while the reconcile loop keeps shedding and waking p1 normally.

### Why not forward-auth

An earlier version used a proxy provider and the `default-authentik` Traefik middleware. It
was the wrong call: nothing else in the fleet used that middleware, and it dragged in an
authentik-managed Ingress competing for the hostname, an outpost assignment whose `providers`
list is a full replacement, and a hard dependency on Traefik being able to resolve cluster
DNS — which it can't, since it runs with `dnsPolicy: None` pointed at the LAN resolver.
In-app OIDC needs none of that.

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
