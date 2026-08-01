# energy-watchdog

Shuts the Proxmox host `p1` down at night when there's no solar surplus, and wakes it
back up in the morning once production covers the load again. The always-on stuff
(network gear, the control-plane Raspberry Pis) keeps running the whole time.

Part of JHC-504. The UPS / power-cut side of things is a separate service (`nut-dog`,
JHC-501), since the trigger and the mechanism are nothing alike.

## How it works

It's a reconcile loop. Every tick it looks at the averaged solar surplus (production
minus consumption), the battery charge, whether `p1` is on, and which guests are
running. That goes into one pure function, `Decide`, which hands back a plan. The
executor carries the plan out, or just logs it when `dryRun` is on. Keeping `Decide`
pure is what lets the whole thing be unit-tested without touching real hardware.

Two things stop it thrashing:

- averaging over a window (`avg_over_time`), so a quick spike from the oven doesn't
  trip a shutdown
- a gap between the shutdown threshold (`shedBelowWatts`) and the wake threshold
  (`headroomWatts`), so it doesn't flip-flop around break-even

### Modes

- `running`: `p1` is on, doing its normal job
- `shed`: `p1` is off. Critical guests got migrated elsewhere, the rest were stopped
- `gaming`: `p1` is on but in shed posture, kept alive because a gaming VM is running

| From | Trigger | What happens |
|------|---------|--------------|
| `running` | deficit, nothing gaming | migrate criticals, stop the rest, silence alerts, power off |
| `running` | deficit, a gaming VM is up | migrate and stop, but leave the host on |
| `shed` | surplus is back | Wake-on-LAN, start the guests it stopped, drop the silence |
| `shed` | host got powered on by hand | treat it as `gaming`, don't fight a manual wake |
| `gaming` | surplus is back | start the stopped guests (host's already on) |
| `gaming` | no gaming VM yet, within grace | leave the host on, wait for a VM |
| `gaming` | no gaming VM once grace elapses, still no surplus | power off |

Migrated guests don't come back on their own. They stay where they landed; moving them
back is a manual call.

### Holding p1 off by hand

Sometimes there's plenty of sun and you still don't want the machine on — a heatwave, or
maintenance. Setting the manual shed holds `p1` down regardless of surplus, via the admin
button in the self-service UI or a ConfigMap key (see below).

It isn't a fourth mode. `Decide` treats it as a permanent deficit, so every rule above
applies unchanged: the gaming guard still keeps the host up, the grace window still runs,
powering `p1` on by hand is still adopted as a gaming session, and a self-service VM request
still wakes it. The one thing that changes is that returning surplus can no longer wake it.

Clearing it hands control straight back to the sun on the next tick.

`energy_watchdog_mode` keeps reporting `shed` while it's on — nut-dog's wake inhibit depends
on that — and `energy_watchdog_manual_shed` says whether it was solar or a person who
decided.

### Desktop VMs (self-service)

Optional UI on `:8080` for starting a desktop VM without touching Proxmox: it wakes `p1`
first if it's off, starts the VM, and shows you the host to connect to once it's up.
Identity comes from authentik, and each VM lists the groups allowed to start it.

There's no one-click hand-off to Moonlight, because no such thing exists: Moonlight has no
URL scheme to launch it with ([moonlight-qt#1874](https://github.com/moonlight-stream/moonlight-qt/issues/1874)
is still open). The UI shows the stream host with a copy button instead.

The UI never touches `p1` itself. It records intent and the reconcile loop acts on it, so
there's still exactly one owner of the host's power state — a click while a shed is halfway
through migrating guests can't fight it. A request that lands during a shutdown simply stays
outstanding until the shutdown finishes, and the loop then wakes the host.

Off unless `selfService.addr` is set. See [DEVELOPMENT.md](./DEVELOPMENT.md) for how it fits
together and how to run the whole thing locally.

### Gaming grace window

When `p1` is on in `gaming` mode but no gaming guest is running yet, it isn't powered off
immediately: a grace window (`gamingGrace`, default 10m) has to elapse first. This covers
two cases:

- You wake `p1` by hand in the middle of a deficit to game. The gaming VM won't autostart,
  so the host would otherwise be seen with no gaming guest and shut straight back down
  before you can launch one.
- A gaming VM reboots mid-session (e.g. GPU-passthrough hiccups). The clock resets every
  time a gaming guest is seen running, so a short reboot rides out the window instead of
  cutting the session short.

Once the window elapses with no gaming guest and no surplus, the host powers off.

### Replication

`p2` and `p3` replicate guests to `p1`. While `p1` is intentionally off those runs fail every
schedule tick and Proxmox mails about each one, so the watchdog disables the replication jobs
that target `p1` for as long as it's down, and re-enables them when it's back (JHC-538).

It flips Proxmox's own `disable` flag — the same thing `pvesr disable` and the GUI's *Enabled*
checkbox set. The job, its schedule and its accumulated replication state all stay put, so
re-enabling picks up incrementally instead of re-syncing from scratch. Nothing is ever
deleted.

Only jobs whose `target` is `proxmox.node` are touched. Jobs *sourced* from `p1` need nothing:
`pvesr` runs on the source node, so while `p1` is off they don't run at all. (A replication
job's `target` is unrelated to `proxmox.targetNodes`, which is only about migration
destinations.)

Which jobs get re-enabled is decided from the jobs themselves, not from stored ids — the same
trick as the silences. On disabling a job the watchdog prefixes its comment with
`[energy-watchdog]`, keeping whatever comment was there behind it, and on the way back up it
only re-enables jobs carrying that prefix, restoring the original comment. So a job you
disabled by hand is never silently switched back on, and a lost state ConfigMap can't strand
replication in the disabled state either.

This runs in `dryRun: alert` too, alongside the silences and for the same reason — that mode
exists to stop a down `p1` making noise. It's the one Proxmox *write* that mode does; it still
moves no guests and never powers the host. `dryRun: true` only logs what it would change. Set
`proxmox.manageReplication: false` to leave replication alone entirely; it defaults to on.

## Guest classes

Three lists. Each entry is a single id (`601`) or a range (`"600-699"`):

- `migrate`: moved off `p1` before it powers down (the Talos node VMs, mostly), spread
  across `targetNodes`, falling back to the next node if one's full
- `stop`: shut down cleanly, remembered, and started again in the morning
- `gamingGuard`: if any of these is running, `p1` stays on

The lists can't overlap, which is checked at startup.

## Config

It all lives in [`config.example.yaml`](./config.example.yaml). In production the
Proxmox token comes from `PROXMOX_TOKEN_ID` / `PROXMOX_TOKEN_SECRET`.

`proxmox.endpoint` points at the proxy (`pve.hla1.jhofer.lan`) that fronts all three
nodes. It stays reachable while `p1` is off because it routes to another node, and any
node can manage `p1`'s guests cluster-internally. TLS verifies against the internal CA
via `caCertPath` (mounted from the jhc-ca image), so `insecureSkipVerify` stays off.

State (the mode and the guests it stopped) goes in a ConfigMap in-cluster, or a local file
when you run it by hand. Alertmanager silences are not stored there: each reconcile lists
the silences it owns (by their `createdBy`) straight from Alertmanager and converges them to
the set it wants, so a lost or stale ConfigMap can never orphan a silence.

When `p1` comes back, its silences aren't dropped the instant the node reports up — the
guests hosted on it (a Talos cluster) take a while to boot, and dropping coverage that early
un-suppresses every one of their still-firing alerts at once. Instead each silence is
shortened to a grace window (`unsilenceGrace`, default 15m) and left to lapse on its own; if
`p1` drops again inside the window the silence is simply extended back out.

## Rolling it out

It ships with `dryRun: true`. In that mode it reads everything and logs what it would
do, but touches nothing. Let it run through a day or two, watch `/metrics` and the logs
against the energy dashboards, and flip `dryRun: false` once it's making the right
calls.

## Before it can actually do its job

- `p1`, `p2`, `p3` need to be one Proxmox cluster with storage the migrate guests can
  move across.
- Wake-on-LAN enabled on `p1`'s NIC in BIOS, and the MAC registered on the node itself
  (`pvenode config set -wakeonlan <mac>`). The watchdog doesn't send the magic packet: it
  calls `POST /nodes/p1/wakeonlan`, so another cluster node broadcasts it on p1's own
  segment. That means the watchdog needs no `hostNetwork` and no particular subnet — but
  it must still run on an always-on node that isn't hosted on `p1`.
- Disable both Proxmox HA and autostart (`onboot=0`) on the `stop` guests. The
  watchdog owns their lifecycle, so nothing else should bring them back. Otherwise, if
  you power `p1` on at night yourself, autostart (or HA) would boot the very VMs the
  watchdog just shut down, and it would have to stop them all over again.
- An API token with `VM.Audit`, `VM.Migrate`, `VM.PowerMgmt`, `Sys.PowerMgmt`, `Sys.Audit` on
  `/nodes` and — for the replication handling — `VM.Replicate` on the replicated guests.
  `Sys.Audit` is what puts `uptime` in `GET /nodes`; without it the field is silently dropped
  and a node still shutting down looks like one you just powered on. Note that
  `GET /cluster/replication` silently omits jobs whose guest the token can't `VM.Audit`, so a
  token that's short on permissions looks like "no replication jobs" rather than an error.
  If you'd rather not grant `VM.Replicate`, set `proxmox.manageReplication: false`.

## Metrics and dashboard

The watchdog serves Prometheus metrics on `:9333/metrics`: the current mode, averaged
surplus, battery charge, p1 power state, gaming-guard state, dry-run flag, and reconcile
health. That's enough to watch what it would do during a dry-run rollout. The Grafana
dashboard for them is
[here](https://github.com/JHOFER-Cloud/fleet-dashboards/blob/main/sync/K8s/Misc/energy-watchdog.json).

## Deploying

There's a Flux deployment example under [`examples/flux`](./examples/flux), with the
namespace, RBAC, config, token Secret shape, and the Deployment. It's a starting point to
adapt, not a drop-in.

## Dev

```sh
go test ./...
go build .
```

To run the whole thing — reconcile loop, API and UI — against a fake Proxmox on your laptop,
see [DEVELOPMENT.md](./DEVELOPMENT.md). It also covers the state/intent split, the
single-owner invariant and how the token verification works.

The image gets built and pushed to `ghcr.io/jhofer-cloud/energy-watchdog` (multi-arch,
arm64 included) by semantic-release when something lands on `main`.
