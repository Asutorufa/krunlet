#!/usr/bin/env python3
"""End-to-end MCP stdio tests on a real libkrun KVM host.

This deliberately verifies guest cancellation and host cleanup through the
actual MCP -> Runner -> libkrun pathway rather than using a fake Executor.
"""

import json
import os
import pathlib
import select
import subprocess
import sys
import tempfile
import time


class MCPClient:
    def __init__(self, binary, rootfs, cgroup, tmpdir):
        env = os.environ.copy()
        env["TMPDIR"] = tmpdir
        self.process = subprocess.Popen(
            [binary, "mcp", "--rootfs", rootfs, "--cgroup-parent", cgroup, "--timeout", "180s"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            bufsize=0,
        )
        self.buffer = b""

    def send(self, message):
        self.process.stdin.write(json.dumps(message, separators=(",", ":")).encode() + b"\n")
        self.process.stdin.flush()

    def read(self, request_id, timeout=90):
        deadline = time.monotonic() + timeout
        while True:
            if b"\n" in self.buffer:
                line, self.buffer = self.buffer.split(b"\n", 1)
                reply = json.loads(line)
                assert reply.get("jsonrpc") == "2.0", reply
                assert reply.get("id") == request_id, reply
                assert "error" not in reply, reply
                return reply["result"]
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"no MCP response to {request_id}")
            ready, _, _ = select.select([self.process.stdout], [], [], remaining)
            if not ready:
                raise TimeoutError(f"no MCP response to {request_id}")
            chunk = os.read(self.process.stdout.fileno(), 65536)
            if not chunk:
                raise RuntimeError(f"MCP process closed stdout before response {request_id}")
            self.buffer += chunk

    def initialize(self):
        self.send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
                   "params": {"protocolVersion": "2025-11-25", "capabilities": {},
                              "clientInfo": {"name": "krunlet-ci", "version": "1"}}})
        reply = self.read(1)
        assert reply["serverInfo"]["name"] == "krunlet", reply
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def call(self, request_id, args):
        self.send({"jsonrpc": "2.0", "id": request_id, "method": "tools/call",
                   "params": {"name": "krunlet_run", "arguments": args}})

    def shutdown(self):
        if self.process.stdin and not self.process.stdin.closed:
            self.process.stdin.close()
        rc = self.process.wait(timeout=20)
        assert rc == 0, f"MCP server exited with status {rc}"
        assert self.buffer == b"" and self.process.stdout.read() == b"", (
            "non-JSON-RPC output escaped onto MCP stdout"
        )

    def abort(self):
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=15)
        if self.process.stderr:
            stderr = self.process.stderr.read()
            if stderr:
                print(stderr.decode(errors="replace"), file=sys.stderr)


def current_roots(tmpdir):
    base = pathlib.Path(tmpdir)
    return {str(p) for prefix in ("krunlet-root-*", "krunlet-state-*", "krunlet-net-*")
            for p in base.glob(prefix) if p.is_dir()}


def current_cgroups(parent):
    return {str(p) for p in pathlib.Path(parent).glob("krunlet-vm-*") if p.is_dir()}


def cgroup_pids(cgroups):
    pids = set()
    for group in cgroups:
        try:
            raw = (pathlib.Path(group) / "cgroup.procs").read_text()
            pids.update(int(pid) for pid in raw.split())
        except FileNotFoundError:
            continue
    return pids


def wait_for_vm(tmpdir, cgroup_parent, previous_roots, previous_groups):
    deadline = time.monotonic() + 75
    while time.monotonic() < deadline:
        roots = current_roots(tmpdir) - previous_roots
        groups = current_cgroups(cgroup_parent) - previous_groups
        pids = cgroup_pids(groups)
        if roots and groups and pids:
            return roots, groups, pids
        time.sleep(0.1)
    raise AssertionError("MCP tool never launched a real VM with rootfs and populated cgroup")


def wait_for_cleanup(tmpdir, cgroup_parent, roots, groups, pids):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        surviving_roots = roots & current_roots(tmpdir)
        surviving_groups = groups & current_cgroups(cgroup_parent)
        surviving_pids = {pid for pid in pids if pathlib.Path(f"/proc/{pid}").exists()}
        if not surviving_roots and not surviving_groups and not surviving_pids:
            return
        time.sleep(0.1)
    raise AssertionError(
        f"orphan resources: roots={surviving_roots}, "
        f"cgroups={surviving_groups}, pids={surviving_pids}"
    )


def main():
    binary = os.environ["KRUNLET_TEST_MCP_BINARY"]
    rootfs = os.environ["KRUNLET_TEST_ROOTFS"]
    parent = os.environ["KRUNLET_TEST_CGROUP_PARENT"]
    with tempfile.TemporaryDirectory(prefix="krunlet-mcp-kvm-") as tmpdir:
        initial_roots = current_roots(tmpdir)
        initial_groups = current_cgroups(parent)
        client = MCPClient(binary, rootfs, parent, tmpdir)
        try:
            client.initialize()
            client.send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
            tools = client.read(2)
            assert [tool["name"] for tool in tools["tools"]] == ["krunlet_run"], tools

            # A real VM runs a shell. Guest stdout must arrive solely inside
            # the MCP tool result, never as raw data on the protocol stream.
            client.call(3, {"command": ["/bin/sh", "-c",
                                      "echo krunlet-mcp-real-vm; echo guest-error >&2"]})
            result = client.read(3)
            assert result["isError"] is False, result
            details = result["structuredContent"]
            assert details["exit_code"] == 0, details
            assert details["stdout"] == "krunlet-mcp-real-vm\n", details
            assert details["stderr"] == "guest-error\n", details
            assert details["containment"] == "cgroup_v2", details

            # Real cancellation while a guest is active. The server must
            # continue reading its stdin and accept ping after cancel.
            client.call(4, {"command": ["/bin/sh", "-c", "sleep 120"]})
            roots, groups, pids = wait_for_vm(
                tmpdir, parent, initial_roots, initial_groups)
            client.send({"jsonrpc": "2.0", "method": "notifications/cancelled",
                         "params": {"requestId": 4, "reason": "KVM CI cancellation"}})
            client.send({"jsonrpc": "2.0", "id": 5, "method": "ping"})
            assert client.read(5) == {}, "server stopped reading while guest was active"
            wait_for_cleanup(tmpdir, parent, roots, groups, pids)

            # EOF must cancel a second running guest and wait for cleanup
            # before the MCP process exits.
            client.call(6, {"command": ["/bin/sh", "-c", "sleep 120"]})
            roots, groups, pids = wait_for_vm(
                tmpdir, parent, initial_roots, initial_groups)
            client.shutdown()
            wait_for_cleanup(tmpdir, parent, roots, groups, pids)
            print("MCP real KVM cancel + EOF cleanup passed")
        finally:
            client.abort()

        # SIGKILL is uncatchable. Restart the server to exercise the
        # startup cleanup path for immediately orphaned rootfs and cgroups.
        killed = MCPClient(binary, rootfs, parent, tmpdir)
        try:
            killed.initialize()
            killed.call(7, {"command": ["/bin/sh", "-c", "sleep 120"]})
            roots, groups, pids = wait_for_vm(
                tmpdir, parent, initial_roots, initial_groups)
            killed.process.kill()
            killed.process.wait(timeout=15)
            restarted = MCPClient(binary, rootfs, parent, tmpdir)
            try:
                restarted.initialize()  # startup recovery finishes before handshake
                wait_for_cleanup(tmpdir, parent, roots, groups, pids)
                restarted.shutdown()
            finally:
                restarted.abort()
            print("MCP real KVM SIGKILL + restart orphan recovery passed")
        finally:
            killed.abort()

        assert current_roots(tmpdir) == initial_roots
        assert current_cgroups(parent) == initial_groups


if __name__ == "__main__":
    main()
