# Docker for DBX

`io.dbx.docker` adds a Docker Engine connection and workbench to DBX. The backend is a Go sidecar using the Docker SDK; the UI is a Vue workbench packaged into a `.dbxp` file.

The workbench lists containers, images, volumes, and networks. It supports container lifecycle and creation, container rename, image pull/push/export, image tagging and per-tag removal, image layer history, live logs, container resource monitoring, container file browsing with upload and download, an interactive container terminal, engine disk usage with pruning, and a controlled subset of Compose YAML. It was adapted from the unmerged DBX Docker workbench branch. The source branch remains available for comparison at [`Ezreal-byte/dbx` (`codex/feature-docker-workbench`)](https://github.com/Ezreal-byte/dbx/tree/codex/feature-docker-workbench).

The UI follows the DBX interface language. It reads `dbxPlugin.locale` after the host initialization handshake and re-applies the language on `dbx-plugin-init` / `dbx-plugin-env`, so switching the DBX language updates the workbench without a reload.

## Requirements and connection security

- DBX 0.6.14 or newer, Host API 1.
- An accessible Docker Engine API. The local socket path is auto-detected per OS (Windows named pipe, macOS/Linux `/var/run/docker.sock`) — leave **Socket path** empty. Local Docker Desktop TCP normally uses `127.0.0.1:2375`; HTTPS normally uses port 2376. Unix sockets and Unix sockets reached through SSH `nc -U` are also supported.
- Plain HTTP to a remote host is rejected unless **Allow insecure remote HTTP** is explicitly enabled. An exposed Docker daemon grants control comparable to the host user or root account. Prefer HTTPS with certificates or an SSH tunnel.
- SSH `nc` connections verify the server key against `~/.ssh/known_hosts` by default. Set **SSH known_hosts path** if the trusted file is elsewhere.
- Read-only connections reject Docker write operations, container terminals, and container file uploads in the Go backend.
- Every RPC handler is wrapped in a panic guard, so a failure inside one method returns an RPC error instead of taking the whole sidecar (and the open workbench) down.

## DBX tunnel reuse

For TCP connections (`http` / `https`) the plugin does not build its own SSH tunnel. When the DBX connection carries an enabled transport layer, DBX resolves the tunnel and delivers the final endpoint in `runtime.host` / `runtime.port`; the plugin dials that endpoint while keeping the configured host in the HTTP `Host` header and TLS SNI. Plain HTTP to a non-loopback host is accepted **because** the tunnel is active, not because the insecure-HTTP switch was flipped.

Unix sockets cannot be expressed as a DBX static TCP forward, so `unix-over-nc` and `unix-over-nc-sudo` remain the plugin's own SSH `nc -U` bridge (with `known_hosts` verification) for that case only.

## Container terminal

Running containers expose a **Terminal** tab backed by `docker exec` with a TTY. The frontend renders an xterm.js terminal and streams keystrokes to the sidecar over the `docker-exec` binary channel; output returns on the same channel. The default command is `/bin/sh` and can be replaced with any argv (for example `/bin/bash` or `psql -U postgres`). Terminals are refused on read-only connections, and on connections marked as production they require an explicit confirmation first.

## Monitoring charts

Container monitoring renders with ECharts (canvas), not a scaled SVG, in a fixed 2x2 grid — CPU % and memory on the first row, network throughput and block I/O on the second. Network and block I/O are derived from Docker's cumulative counters with per-second rates, and counter resets (a container restart) are clamped to zero instead of producing a negative spike. A summary row shows the latest sample in numbers.

## Files in a container

The **Files** tab browses read-only, and additionally supports **download** (per file) and **upload** (multi-select, into the current directory). Downloads stream over the `docker-file` binary channel and land through the same native save dialog as image export; uploads stream the local file's bytes into the container as `docker exec` stdin, where a fixed `head -c <size> > <path>` script writes it. Both directions are capped at 256 MiB, require `/bin/sh`, and uploads additionally require `head`.

## Image export and where files land

Image export asks the desktop host for a native save dialog and the host returns the absolute path it wrote. Every finished download keeps that path in the transfer record, so the **Transfers** popover shows the destination directory and a one-click **copy path** button instead of leaving the file location unknown. The web host cannot hand a client path to the plugin, so there the record shows the browser-download hint instead of a directory.

A plugin sandbox has no host API to reveal a path in the OS file manager (`reveal_path_in_file_manager` is app-internal), so the plugin copies the path rather than opening Explorer.

## Disk usage and cleanup

The header opens a `docker system df` view with per-category size and reclaimable space for images, containers, volumes, build cache, and networks.

Cleanup is deliberately hard to trigger by accident:

1. It lives in a **modal dialog**, not a one-click button.
2. Picking a category runs a **read-only preview** (`docker/prunePreview`) that lists exactly what would be deleted — names, ids and sizes — with a per-category note explaining what is kept.
3. The delete button stays disabled until the user **types the confirmation phrase** (`我同意` / `I AGREE`) exactly.
4. After running, the actual deleted count is compared with the preview and any mismatch is reported.

Targets mirror the Docker CLI defaults instead of "delete as much as possible":

| Target | Removes | Keeps |
| --- | --- | --- |
| Stopped containers | every container not running or paused | running / paused containers |
| Dangling images | untagged images no container references | tagged images |
| Unused images (`all`) | every image no container references, tagged or not | images in use |
| Unused anonymous volumes | same as `docker volume prune` | named volumes |
| Unused volumes (`all`) | every volume no container uses, incl. named | volumes in use |
| Unused networks | networks with no container attached | predefined Docker networks |

The preview needs the same capabilities as the operation itself (`/bin/sh` is not required; it only uses the Docker API). A destructive action still requires a writable connection.

## Limitations

The plugin does not include a Docker daemon or the Docker CLI. The Compose editor supports common image, environment, port, volume, network, restart, label, and command fields. It does not implement builds, `.env`, `depends_on` health checks, or arbitrary Compose extensions. File browsing requires `/bin/sh`, `stat`, and `head` in the container, and the container terminal requires the requested command to exist inside the container (distroless images without a shell cannot open one). Image export is buffered in the workbench, so very large archives may exceed available memory.

## Build and local verification

Requires Node.js 20 or newer and Go 1.26 or newer.

```powershell
npm ci --ignore-scripts
npm test
go -C backend test ./...
go -C backend/sdk test ./...
go -C backend vet ./...
npm run package
```

The end-to-end smoke test drives the built sidecar over the framed protocol and needs a reachable Docker Engine:

```powershell
go -C backend build -o ../dist/verify-sidecar.exe .
node scripts/smoke-sidecar.mjs dist/verify-sidecar.exe http://127.0.0.1:2375 <running container name>
```

It verifies the handshake, direct and tunnel-routed connections, the remote plain-HTTP guard, resource listing, log streaming, read-only file browsing, the interactive terminal (stdin, resize, exit), a container file upload/download byte-for-byte round-trip, the read-only terminal/upload guards, disk usage, image tag/untag round-trip, image layer history, and container rename round-trip.

The container rename and image tag checks restore the original state in a `finally` block, the file round-trip removes its temporary file, and pruning is skipped unless `SMOKE_ALLOW_PRUNE=1` is set — pruning really deletes stopped containers.

On Windows x64, the package is `dist/io.dbx.docker-0.1.4-windows-x64.dbxp`. It is an **unsigned review candidate**. To test it in DBX, enable **Allow unsigned development package** in Plugin Center, then install the `.dbxp` locally. Normal marketplace installation requires DBX Store review and signing.

Publishing a GitHub Release triggers `.github/workflows/release.yml` to build separate native candidates for Windows, macOS, and Linux. Release assets and `release-candidates.json` are the inputs to a candidate PR in [`t8y2/dbx-store`](https://github.com/t8y2/dbx-store). The store signs approved bytes; this repository contains no signing key.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
