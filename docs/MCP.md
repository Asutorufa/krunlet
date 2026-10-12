# MCP server (stdio)

Krunlet includes a Model Context Protocol (MCP) server for agents that need to run
commands in disposable libkrun microVMs. The MCP transport is **stdio**, so the
client launches a local `krunlet` process; there is no HTTP listener, daemon,
API key, or TCP port.

## Start

Install an official release binary or build locally:

```sh
CGO_ENABLED=0 go build -o krunlet ./cmd/krunlet
./krunlet mcp --rootfs /absolute/path/to/trusted/rootfs
```

The rootfs must already exist and contain the Linux executables your agent
will run. Krunlet does not download or construct a rootfs automatically.
Host libkrun/libkrunfw requirements, release bundle behavior, and macOS
Hypervisor entitlement requirements are the same as for `krunlet run`.
Check `krunlet doctor` before configuring your client.

### Client config (JSON-style MCP clients)

Configure your agent's MCP server list with an absolute executable path:

```json
{
  "mcpServers": {
    "krunlet": {
      "command": "/absolute/path/to/krunlet",
      "args": [
        "mcp",
        "--rootfs", "/absolute/path/to/trusted/rootfs",
        "--timeout", "30s",
        "--max-concurrent-vms", "2"
      ]
    }
  }
}
```

For Codex clients using TOML configuration, the equivalent entry is:

```toml
[mcp_servers.krunlet]
command = "/absolute/path/to/krunlet"
args = ["mcp", "--rootfs", "/absolute/path/to/trusted/rootfs", "--timeout", "30s"]
```

On **Linux hosts running untrusted agent code**, supply a dedicated,
administrator-delegated cgroup v2 parent:

```sh
krunlet mcp --rootfs /absolute/path/to/rootfs \
  --cgroup-parent /sys/fs/cgroup/krunlet
```

See [SECURITY.md](../SECURITY.md) for cgroup controller requirements,
host-side helper containment, and remaining hypervisor/virtio-fs risks.
Run as an unprivileged user. The server prints warnings and errors to
**stderr**; stdout is reserved exclusively for MCP JSON-RPC messages.
Clients should not redirect stderr into stdout.

## Exposed tool

The server declares the `krunlet_run` tool. A call starts a fresh microVM
using the trusted startup configuration:

```json
{
  "name": "krunlet_run",
  "arguments": {
    "command": ["/bin/sh", "-lc", "cat /tmp/input.txt > /tmp/output.txt"],
    "workdir": "/",
    "stdin": "",
    "env": {"LANG": "C"},
    "files": {"/tmp/input.txt": "aGVsbG8K"},
    "collect": ["/tmp/output.txt"],
    "timeout_ms": 10000
  }
}
```

- `command`: required guest executable and argument array. Shell features
  need an explicit `/bin/sh -lc`; arguments are **not** host-shell commands.
- `workdir`, `files`, and `collect` refer to **absolute guest paths**.
  `files` contains base64 strings, not host-side filenames.
- `stdin` and `env` are guest data. The runner does not inherit the entire
  host environment.
- `timeout_ms` optionally sets a shorter deadline than the launcher's
  `--timeout` cap. It cannot increase the cap.
- The result contains `exit_code`, `stdout`, `stderr`, `duration_ms`,
  `timed_out`, `output_limited`, `run_id`, and `containment`. Collected
  files are returned as base64. An execution error is returned as a normal
  MCP tool result with `isError: true`, not a broken transport response.
- MCP clients that support structured results receive `structuredContent`;
  other clients can parse the JSON text content.

The server enforces the Runner's output and file limits, a maximum 8 MiB
request line, and bounds on the number of environment variables and files.
Tool calls execute concurrently up to the Runner's VM limit. MCP
`notifications/cancelled`, client disconnect/EOF, and process shutdown
cancel in-flight calls so the Runner can clean up their VM helpers.

## Safety defaults and host-only flags

- **Network disabled by default.** The MCP server will **not** expose
  unrestricted libkrun TSI to untrusted agents.
- A trusted operator can enable only restricted gVisor networking using
  `--network --allow-cidr IP_OR_CIDR[,PORT[,tcp|udp]]`, optionally repeated,
  and `--block-cidr`. Private/local IP destinations are blocked by default
  while networking is enabled. Enabling this requires a libkrun 1.19.x build
  with `NET=1`.
- **Persistent mode is not available.** Each execution stages a private
  copy of the selected trusted rootfs.
- The agent cannot set rootfs, libkrun library, network policy, resources,
  VM concurrency limits, host cgroup parent, or host file paths through
  tool arguments.
- The MCP server process still holds the privileges of the account running
  it. A microVM is not a substitute for host OS confinement, storage quotas,
  or supervisor-enforced process cleanup.
- The MCP protocol is not an authorization layer. Only configure this
  process in agent clients you trust to invoke the exposed tool.

The implementation supports newline-delimited JSON-RPC 2.0 over stdio,
`initialize` with legacy MCP version negotiation through `2025-11-25`,
`ping`, `tools/list`, `tools/call`, and cancellation notifications. It
does not advertise MCP resources, prompts, HTTP transports, or 2026
task-augmented execution.
