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
- Wake-on-LAN on for `p1`'s NIC, and the watchdog running with `hostNetwork` on an
  always-on node on the same segment so the magic packet lands.
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
namespace, RBAC, config, token Secret shape, and a hostNetwork Deployment. It's a
starting point to adapt, not a drop-in.

## Dev

```sh
go test ./...
go build .
```

The image gets built and pushed to `ghcr.io/jhofer-cloud/energy-watchdog` (multi-arch,
arm64 included) by semantic-release when something lands on `main`.
