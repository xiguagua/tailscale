- right now, machines hardcoded to 4, range hardcoded to "100.64.2.0/26", "100.64.2.64/26", "100.64.2.128/26", "100.64.2.192/26"
Operator creates a StatefulSet with 4 replicas for an applied ClusterConfig

- do allowed IPs work?
`tailscale debug prefs` on proxies show the right advertizeroutes

on my client `t status --json | jq -r '.Peer'` shows that the nodes have the right allowedIPs
```
 "AllowedIPs": [
      "100.64.1.205/32",
      "fd7a:115c:a1e0::a901:8838/128",
      "100.64.2.64/26"
    ],
```

Separate ConfigMap for each proxy replica, each replica gets mounted all of them at service-<N> where `N` is the ordinal of the replica

^ And those seem to persist even when --accept-routes is off on the client- because these are CGNAT?

v0.0.14proxycidr - current operator tag
v0.0.15proxycidr - proxy tags

need to --accept-routes on client for this to work
TODO: can we bypass that?

next steps:
- when a new ingress Service exposed, update service ConfigMaps
- in proxies, read ConfigMaps, update firewall rules
- test ^
- add a DNS reconciler that reads ConfigMap contents and configures some nameserver records (could for now use Secrets for proxy IPs)
- test ^
- configure DNS by hand/split DNS?
- test ^

## authentication problem