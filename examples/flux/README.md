# Flux deployment example

A self-contained example of running energy-watchdog with Flux. It's a starting point,
not a drop-in: fill in the `TODO` values and adapt it to your cluster.

- `flux-kustomization.yaml`: the `GitRepository` + Flux `Kustomization` that point Flux
  at this directory. The rest of the files are the kustomize base it deploys.
- `kustomization.yaml`: the kustomize entrypoint listing the resources below.
- `namespace.yaml`, `rbac.yaml`: the `energy` namespace and the ServiceAccount/Role that
  let the watchdog read and write its one state ConfigMap.
- `configmap.yaml`: the config, mounted at `/config/config.yaml`, with `dryRun: true`.
- `secret.example.yaml`: the shape of the Proxmox token Secret. Don't commit a real one,
  encrypt it or sync it from a secret manager.
- `deployment.yaml`: the Deployment (hostNetwork + `ClusterFirstWithHostNet` for WoL and
  cluster DNS), Service, and an optional ServiceMonitor.

Before applying, fill in: the always-on `nodeSelector` host, p1's `mac`, `targetNodes`,
the CA source (the `jhc-ca` ConfigMap), and a real token Secret. Leave `dryRun: true`
until you've watched it make the right calls for a day or two.
