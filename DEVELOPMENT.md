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
enough to look at. Guest start/stop is instant; pass `-task-delay=40s` to make bulk migrate and
stop tasks slow, which is what puts the UI into its mid-shed states.

Preloaded guests: `101`/`102` (migrate class), `301`/`302` (stop class), `601`/`602` (gaming
guard / desktop VMs). Two in each class so a shed exercises the round-robin across
`targetNodes` and a mixed qemu/lxc bulk stop. Both desktop VMs report the same `hostpci0`
passthrough device, so the UI's one-guest-per-GPU block is reachable locally.

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
| `intent.json` | the API, or you with kubectl | manual holds, wake requests (spec) |

They are written by **separate merge patches**, never a whole-object PUT. That matters: a
migrate can run 15 minutes, and a state save landing at the end of it must not wipe an intent
set while it was in flight. `TestConfigMapStoreKeysAreIndependent` pins this down.

The loop never writes intent. Wake requests therefore aren't cleared when satisfied — they
age out after `gamingGrace`. By then either the VM is up, and the *gaming guard* is what
holds p1, or it failed and retrying is the user's call.

The TTL is `gamingGrace` rather than a constant of its own on purpose: a request holds p1 up
for exactly as long as a gaming session gets to produce a running VM, so tuning one can't
open a window where a request outlives the grace that honours it.

### Manual holds

`Decide` pins the signal rather than adding a mode:

```go
switch {
case s.ManualShed:
    sig = sigDeficit
case s.ManualOn:
    sig = sigSurplus
}
```

That is the whole feature, both directions. Every existing rule then applies unchanged —
gaming guard still vetoes the power-off, the grace window still runs, powering p1 on by hand
is still adopted — while the opposite signal becomes unreachable, so solar can neither wake a
held-off node nor shed a held-on one.

The shed case is deliberately first: the API writes the pair through one endpoint and can't
set both, but a hand-edited `intent.json` can, and the heatwave outranks the convenience.

**`energy_watchdog_mode` deliberately keeps reporting `shed`, not `manual`.** A fourth mode
value would break every dashboard and alert that keys off it, and the "why" belongs in the
separate `energy_watchdog_manual_shed` / `energy_watchdog_manual_on` gauges rather than in
the mode itself.

### p1's power belongs to nut-dog

This loop decides *whether* p1 should be on; nut-dog does the powering. It has to work when
the cluster doesn't, and its mechanisms need neither Proxmox nor a second node — where this
loop's own WoL goes through `POST /nodes/pve-1/wakeonlan`, i.e. it needs another node up to
relay the packet, which is exactly what a full shed leaves you without.

So `wake` and the power-off become one call each (`powerAPI`), and the wish is **restated
every tick** rather than sent on transitions: nut-dog keeps requests in memory, and its
`startupGrace` is sized so a restart is covered by the next tick here.

`powerWish` derives that wish from what the loop actually established — never from the mode
label. `ModeRunning` with p1 down is a deliberate hold, and `ModeShed` with p1 up is the
stalemate where a hand-started node is left alone; asserting a wish in either case powers p1
against the loop's own decision. Both are pinned by `TestRestateNeverOverridesTheLoop`.

There is no gate in the other direction. nut-dog resolves the conflict itself: a critical UPS
outranks any request, so this side never has to ask permission, and the two can't deadlock.
Watch `energy_watchdog_power_request_success` — with all of p1's power running through that
call, a wrong token or load name is otherwise silent.

A hold-on is a latch with no expiry, so alert on
`min_over_time(energy_watchdog_manual_on[6h]) == 1` — a forgotten one means p1 runs on grid
power every night.

### Break-glass

If the UI is down, intent is just a ConfigMap key:

```sh
kubectl -n energy patch cm energy-watchdog-state --type merge \
  -p '{"data":{"intent.json":"{\"shed\":true}"}}'   # or {"on":true}
```

The loop picks it up on the next tick. Write `{}` to hand control back to the sun.

## Regenerating the decision reference

There is a published page listing every decision this loop can reach and every request nut-dog
can honour or refuse. It is worth keeping, because it is not written by hand: both tables come
from running the real `Decide` and `DesiredForLoad` over their whole input space, so the page
cannot claim behaviour the code doesn't have. It has already earned that twice — it is how the
`running` + p1-down dead end surfaced, and how `hold` turning into `on` inside nut-dog was
caught.

The generators live as tests that skip unless `OUT` is set, so they cost nothing in a normal
run but still compile against the real signatures:

```sh
# every reachable Decide outcome (274 rows)
OUT=/tmp/decide.json go test ./internal/controller/ -run TestGenerateDecisionTable

# nut-dog's precedence + reconcile tables (40 + 27 rows), from the nut-dog repo
OUT=/tmp/nutdog.json go test ./internal/control/ -run TestGenerateNutDogTables
```

Both dump JSON. The page is a self-contained HTML artifact built from those two files: a
filterable table per dataset, plus two hand-drawn SVGs (the delegation path and the mode state
machine). Rebuilding it is a matter of re-rendering that JSON — ask Claude to rebuild the
decision reference from the two dumps, and give it the existing artifact URL so it updates in
place instead of minting a new one.

When adding a dimension to `Decide` — a new mode, another intent flag — add it to the loop in
`decisiontable_test.go` too, or the page quietly stops covering it.

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
