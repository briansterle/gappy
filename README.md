# gappy

A fast, lightweight tool for packing container images and Helm charts into a portable OCI store and serving them in air-gapped environments.

## Why gappy

| | gappy | hauler |
|---|---|---|
| Binary size | ~10 MB | ~100 MB |
| Dependencies | `go-containerregistry` only | helm SDK, k8s client-go, ... |
| Helm repo serving | Single port (OCI + HTTP) | Separate |
| Skip cached artifacts | Yes (digest check) | Yes |
| Hauler manifest support | Yes | Yes |

gappy is purpose-built for a single job: pack images and charts on a connected machine, carry the store across the air gap, serve everything locally. It does not try to be a general-purpose artifact manager.

## Install

```bash
go install github.com/briansterle/gappy@latest
```

Or build from source:

```bash
bash build.sh
cp gappy ~/bin/
```

Requires Go 1.21+.

## Commands

```
gappy [-j N] pack <images.txt|manifest.yaml>        pack container images
gappy [-j N] pack-charts <charts.txt|manifest.yaml>  pack Helm charts
gappy serve [store-path]                             serve images + charts
gappy discover [dir]                                 find image and chart refs
gappy version                                        print version info
```

`-j N` controls parallel download workers (default: CPU count - 1).

## Workflow

### 1. Discover

Scan a directory tree for container image references and Helm chart dependencies:

```bash
gappy discover templates     # finds image refs → found-images.txt
gappy discover charts        # finds chart refs → found-charts.txt
```

### 2. Pack images

Accepts a plain text file (one image per line) or a Hauler image manifest:

```bash
gappy -j 12 pack hauler/hauler-image-manifest.yaml
gappy -j 12 pack found-images.txt
```

Images are stored in an OCI layout at `./store`.

Hauler manifest format (`kind: Images`):

```yaml
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: my-images
spec:
  images:
    - name: nginx:alpine
    - name: redis:alpine
```

### 3. Pack charts

Accepts a Hauler chart manifest or the `found-charts.txt` produced by `discover`:

```bash
gappy pack-charts hauler/hauler-chart-manifest.yaml
gappy pack-charts found-charts.txt
```

Hauler manifest format (`kind: Charts`):

```yaml
apiVersion: content.hauler.cattle.io/v1
kind: Charts
metadata:
  name: my-charts
spec:
  charts:
    - name: activemq
      version: 6.1.6-stig-increased-resources
      repoURL: rs-dev-helm
    - name: cert-manager
      version: v1.14.0
      repoURL: rs-dev-helm-oci
```

`repoURL` is a Helm repo alias name. gappy resolves aliases via `~/.config/helm/repositories.yaml` — the same file Helm itself uses. HTTP repos are downloaded as `.tgz` files; OCI repos are pulled into the OCI layout.

Authentication for both image registries and Helm repos uses the same credentials:

```bash
export RSART_LOCAL_USER=myuser
export RSART_LOCAL_AUTH=mypassword
```

### 4. Serve

```bash
gappy serve              # uses ./store
gappy serve /path/store  # explicit path
```

A single server on `:5000` handles everything:

| Path | Protocol | Content |
|---|---|---|
| `/v2/...` | OCI Distribution | Container images |
| `/{repoName}/index.yaml` | Helm HTTP | Chart index |
| `/{repoName}/{chart}-{version}.tgz` | Helm HTTP | Chart package |

Helm HTTP repos are auto-discovered from subdirectories of `./store/helm/` at startup. In the air gap:

```bash
helm repo add rs-dev-helm http://localhost:5000/rs-dev-helm
helm pull rs-dev-helm/activemq --version 6.1.6-stig-increased-resources
```

## Store layout

```
store/
├── blobs/sha256/        OCI content-addressable blobs (images + OCI charts)
├── index.json           OCI layout index
├── oci-layout
└── helm/
    └── rs-dev-helm/     HTTP Helm repo charts
        ├── activemq-6.1.6-stig-increased-resources.tgz
        └── ...
```

## Skip-if-cached

`pack` and `pack-charts` check whether a blob already exists in the store before pulling. Re-running against an unchanged manifest is fast — only new or updated artifacts are downloaded.

## Version

```bash
gappy version
# gappy v0.0.5
#   commit:  b64fd24
#   built:   2026-05-29T18:43:00Z
#   go:      go1.26.3
#   os/arch: linux/amd64
```

Build stamps are injected by `build.sh` via `-ldflags`.
