# Fixing `exec format error` / failed image pulls on AlmaLinux WSL

If `gappy pack`/`gappy web` (or `docker pull`) fails with `401 Unauthorized`,
`DENIED: Not Authorized`, or you see `exec format error:
docker-credential-wincred.exe`, the cause is usually **WSL `.exe` interop being
disabled by systemd** — not bad credentials.

## Symptoms

- `gappy` pulls fail with `fetch failed: ... 401 incorrect username or password`
  (docker.io) or `DENIED: Not Authorized` (other registries).
- Running the credential helper directly errors:
  ```
  $ printf 'https://index.docker.io/v1/' | docker-credential-wincred.exe get
  exec format error: docker-credential-wincred.exe
  ```
- It works fine in PowerShell and in your other WSL distros, and only
  *sometimes* breaks on AlmaLinux.

## Root cause

`~/.docker/config.json` has a global `"credsStore": "wincred.exe"`, so every
registry lookup shells out to the Windows helper `docker-credential-wincred.exe`.
That helper is a Windows binary; running it from Linux requires WSL's `.exe`
interop, which is the `WSLInterop` handler in `binfmt_misc`.

This distro enables systemd (`/etc/wsl.conf` → `[boot] systemd=true`). On boot,
WSL registers `WSLInterop` early, then systemd's binfmt machinery
(`systemd-binfmt.service`) **flushes `binfmt_misc` and re-applies only what's in
`/etc/binfmt.d/` and `/usr/lib/binfmt.d/`** — which are empty — wiping the WSL
handler. Linux can then no longer exec any `.exe`, so the credential helper dies
with `exec format error` and pulls fail. The boot-timing race is why it's
intermittent here but fine on non-systemd distros (and irrelevant in native
PowerShell).

Confirm the handler is missing:

```bash
cat /proc/sys/fs/binfmt_misc/WSLInterop   # "No such file or directory" == broken
```

## Fix A — restore WSL interop (fixes the helper and every other `.exe`)

Immediate (in-memory), then verify:

```bash
sudo sh -c 'echo ":WSLInterop:M::MZ::/init:PF" > /proc/sys/fs/binfmt_misc/register'
printf 'https://index.docker.io/v1/' | docker-credential-wincred.exe get   # JSON now, not "exec format error"
```

Persistent across reboots (systemd-native — the real fix):

```bash
echo ':WSLInterop:M::MZ::/init:PF' | sudo tee /etc/binfmt.d/WSLInterop.conf
sudo systemctl restart systemd-binfmt.service
cat /proc/sys/fs/binfmt_misc/WSLInterop   # should show: enabled ... interpreter /init
```

Adding the `binfmt.d` file satisfies `systemd-binfmt`'s
`ConditionDirectoryNotEmpty`, so the handler is re-registered on every boot.

If systemd still clobbers it on some boots, add a boot command to
`/etc/wsl.conf`, then `wsl --shutdown` from PowerShell:

```ini
[boot]
systemd=true
command = /bin/sh -c 'test -e /proc/sys/fs/binfmt_misc/WSLInterop || echo ":WSLInterop:M::MZ::/init:PF" > /proc/sys/fs/binfmt_misc/register'
```

## Fix B — decouple credentials from interop (makes pulls work regardless)

The only credential gappy actually needs (`artifactory.dle.afrl.af.mil`) is
already stored **inline under `auths`** in `~/.docker/config.json`. The global
`"credsStore": "wincred.exe"` only adds the flaky Windows helper for *every*
registry, including public ones that should be anonymous. Drop it:

```bash
cp ~/.docker/config.json ~/.docker/config.json.bak
jq 'del(.credsStore)' ~/.docker/config.json.bak > ~/.docker/config.json
```

Now artifactory uses its inline `auth`, and docker.io / public.ecr.aws go
anonymous — no `.exe` exec required.

Caveat: `docker login` or Rancher Desktop may re-add `credsStore`. If so, scope
the helper to only the registries that need it with `credHelpers` instead of a
global `credsStore`, or use a clean config per command:

```bash
DOCKER_CONFIG=$(mktemp -d) gappy web      # forces anonymous, ignores the keychain
```

## Recommendation

Apply **both**: Fix B unblocks pulls immediately; Fix A restores interop health
for `docker login`, other `.exe` tooling, etc.

## Note on secrets

The `auth` value in `config.json` is base64(`user:token`), not encryption —
anyone who reads the file recovers the token. Avoid pasting the raw config into
shared logs/transcripts, and rotate the Artifactory token if it leaks.
