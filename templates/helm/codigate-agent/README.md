# codigate-agent Helm chart

Runs the [Codigate agent](https://github.com/codigate/agent) in Kubernetes (k3s, k0s, kind, EKS, AKS, GKE and others, version 1.25 or newer). The agent opens one outbound TLS connection to the Codigate edge and forwards requests to Services in your cluster. It needs no Ingress, no LoadBalancer and no open ports.

The chart is published as an OCI artifact: `oci://ghcr.io/codigate/charts/codigate-agent`. Its version is the agent version, and by default it runs the image `ghcr.io/codigate/agent:v<version>`.

## Install

1. Create an agent in the dashboard and copy its token (`cga_…`).
2. Store the token in a Secret, so it never ends up in your shell history or in the Helm release:

   ```sh
   kubectl create namespace codigate
   kubectl -n codigate create secret generic codigate-token --from-literal=token='cga_…'
   ```

3. Write `values.yaml` with your ingress rules. Point them at Services in the cluster by their DNS name:

   ```yaml
   existingSecret: codigate-token
   config:
     ingress:
       - hostname: grafana.example.com
         service: http://grafana.monitoring.svc:80
       - hostname: git.example.com
         service: http://gitea-http.gitea.svc:3000
       - hostname: ssh.example.com
         service: ssh://gitea-ssh.gitea.svc:22
       - service: http_status:404
   ```

   The last rule must have no hostname. Every hostname also needs an app in the dashboard.

4. Install. Use the agent version from the [releases page](https://github.com/codigate/agent/releases) as the chart version:

   ```sh
   helm install codigate-agent oci://ghcr.io/codigate/charts/codigate-agent \
     --version 0.2.0 --namespace codigate -f values.yaml
   ```

5. Check that it connects:

   ```sh
   kubectl -n codigate logs deploy/codigate-agent -f
   ```

   `tunnel connected` means you are done. `tunnel disconnected, reconnecting` means the agent cannot reach the edge or the token was rejected.

Upgrade with `helm upgrade codigate-agent oci://ghcr.io/codigate/charts/codigate-agent --version <new> -n codigate -f values.yaml`. Changes to `config` or `token` restart the pods automatically. If you use `existingSecret` or `existingConfigMap`, run `kubectl rollout restart` after changing them.

For a quick test you can pass the token directly with `--set token=cga_…`. The chart then creates the Secret itself. The token is stored in the Helm release as well.

## Values

| Value | Default | Description |
| --- | --- | --- |
| `existingSecret` | `""` | Secret that holds the token. Recommended. |
| `existingSecretKey` | `token` | Key in that Secret. |
| `token` | `""` | Token, used when `existingSecret` is empty. The chart creates a Secret. |
| `edge` | `https://connect.codigate.nl` | Edge the agent connects to. |
| `config` | catch-all only | The agent config (same format as [codigate.example.yml](https://github.com/codigate/agent/blob/main/codigate.example.yml)), rendered into a ConfigMap. Do not put the token or `edge` in it. |
| `existingConfigMap` | `""` | Use your own ConfigMap instead of `config`. |
| `existingConfigMapKey` | `codigate.yml` | Key in that ConfigMap. |
| `replicaCount` | `1` | Agent pods. Every pod uses the same token and the edge spreads requests over them. |
| `image.repository` | `ghcr.io/codigate/agent` | Image. |
| `image.tag` | `v<appVersion>` | Image tag. |
| `image.digest` | `""` | Pin by digest (`sha256:…`), used instead of the tag. |
| `resources` | 10m CPU, 32Mi request, 128Mi limit | Container resources. |
| `podDisruptionBudget.enabled` | `false` | Create a PodDisruptionBudget (`minAvailable: 1` or set `maxUnavailable`). Useful with 2 or more replicas. |
| `serviceAccount.create` | `true` | Create a ServiceAccount. Its token is never mounted; the agent needs no Kubernetes API access. |
| `podSecurityContext`, `securityContext` | restricted | Non-root (uid 65532), read-only root file system, all capabilities dropped, seccomp `RuntimeDefault`. Meets the Pod Security Standard *restricted*. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | empty | Scheduling. |
| `podAnnotations`, `podLabels` | `{}` | Extra pod metadata. |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | `[]` | Extra environment variables and volumes for the agent container. |

[values.yaml](./values.yaml) has comments for every value. [values.schema.json](./values.schema.json) rejects unknown values and malformed rules before anything is installed.

## Health checks

The agent has no HTTP endpoint or listening port, so the chart sets no liveness or readiness probes. The agent never exits on a connection error. It keeps retrying with backoff, so a crash loop always points to a real problem, such as an invalid config. Watch the log or the agent's status in the dashboard.

## Docker discovery

Docker labels do not apply in Kubernetes. Leave `docker` out of `config` and write the rules yourself.

## Testing the chart

[`../test.sh`](../test.sh) runs `helm lint` and `helm template` checks. `../test.sh install` installs the chart in the current kubectl context with a dummy token. It checks that the pods run and that the agent starts and tries to connect. CI runs both on a k3s cluster made with k3d.
