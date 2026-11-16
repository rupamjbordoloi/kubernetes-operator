Here's one setup that you do **once per Mac**. After that, every `kind create cluster` works behind Zscaler with no extra steps, and it keeps working across kind upgrades because there's no custom image.

**The idea:** create one CA bundle file containing the normal public CAs plus Zscaler's. Then trust it in the Podman VM, and have kind **mount it into every node** as the node's trusted CA list.

## One-time setup (run on the Mac)

**1. Export the Zscaler certificate from the keychain (PEM):**

```bash
security find-certificate -a -c "Zscaler" -p /Library/Keychains/System.keychain > ~/zscaler.crt
```

**2. Build the CA bundle: the Mac's public CAs plus Zscaler:**

```bash
cat /etc/ssl/cert.pem ~/zscaler.crt > ~/.kind-ca-bundle.crt
```

**3. Trust Zscaler in the Podman VM** (it stays there across restarts):

```bash
podman machine ssh "sudo tee /etc/pki/ca-trust/source/anchors/zscaler.crt > /dev/null && sudo update-ca-trust" < ~/zscaler.crt
```

**4. Write a kind config that mounts the bundle into every node:**

```bash
cat > ~/kind-config-zscaler.yaml <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts: &ca
      - hostPath: $HOME/.kind-ca-bundle.crt
        containerPath: /etc/ssl/certs/ca-certificates.crt
        readOnly: true
  - role: worker
    extraMounts: *ca
  - role: worker
    extraMounts: *ca
EOF
```

## Every time you create the cluster

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
```

```bash
kind create cluster --name drishti --config ~/kind-config-zscaler.yaml
```

That's it. Image pulls work right away: no `update-ca-certificates`, no containerd restart, no `kind load`. The cluster is the same as with `yaml/kind-config.yaml` (1 control plane + 2 workers), just with the CA mount added.

To make the `export` permanent too:

```bash
echo 'export KIND_EXPERIMENTAL_PROVIDER=podman' >> ~/.zshrc
```

## Quick check

```bash
podman exec drishti-control-plane crictl pull docker.io/otel/opentelemetry-collector-contrib:0.165.0
```

## Why it's permanent

- **Podman VM:** the certificate is on the VM's disk, so it survives restarts and Mac reboots. Redo step 3 only if you ever run `podman machine rm`.
- **kind nodes:** the bundle is mounted from your Mac when each node starts, so every new cluster gets it automatically. Podman shares your home folder with its VM by default, which is what makes `$HOME/...` work.
- **Kind upgrades:** nothing to rebuild, since the stock node image is used.
- **When Zscaler rotates its certificate** (rare): rerun steps 1–3 and recreate the cluster.

Don't commit `~/zscaler.crt` or the bundle to the repo: they're specific to your company's network.