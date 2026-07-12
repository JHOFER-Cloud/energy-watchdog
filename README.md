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
| `gaming` | gaming's done, still a deficit | power off |

Migrated guests don't come back on their own. They stay where they landed; moving them
back is a manual call.

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
- An API token with `VM.Audit`, `VM.Migrate`, `VM.PowerMgmt` and `Sys.PowerMgmt`.

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
