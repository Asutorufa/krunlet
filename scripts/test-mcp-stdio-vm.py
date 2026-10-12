#!/usr/bin/env python3
"""Real libkrun smoke test for the stdio MCP contract (run in the KVM CI job)."""

import json
import os
import select
import subprocess
import sys


def main():
    binary = os.environ["KRUNLET_TEST_MCP_BINARY"]
    rootfs = os.environ["KRUNLET_TEST_ROOTFS"]
    cgroup = os.environ["KRUNLET_TEST_CGROUP_PARENT"]
    process = subprocess.Popen(
        [binary, "mcp", "--rootfs", rootfs, "--cgroup-parent", cgroup, "--timeout", "60s"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    def exchange(message):
        process.stdin.write(json.dumps(message, separators=(",", ":")) + "\n")
        process.stdin.flush()
        ready, _, _ = select.select([process.stdout], [], [], 90)
        if not ready:
            raise TimeoutError("timed out waiting for MCP JSON-RPC response")
        line = process.stdout.readline()
        if not line:
            raise RuntimeError("MCP server closed stdout prematurely")
        reply = json.loads(line)
        assert reply.get("jsonrpc") == "2.0", reply
        assert reply.get("id") == message["id"], reply
        assert "error" not in reply, reply
        return reply["result"]

    try:
        initialized = exchange({
            "jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2025-11-25", "capabilities": {},
                       "clientInfo": {"name": "krunlet-ci", "version": "1"}},
        })
        assert initialized["serverInfo"]["name"] == "krunlet", initialized

        process.stdin.write('{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
        process.stdin.flush()

        tools = exchange({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        assert [tool["name"] for tool in tools["tools"]] == ["krunlet_run"], tools

        result = exchange({
            "jsonrpc": "2.0", "id": 3, "method": "tools/call",
            "params": {"name": "krunlet_run", "arguments": {
                "command": ["/bin/sh", "-c", "echo krunlet-mcp-real-vm"]
            }},
        })
        assert result["isError"] is False, result
        details = result["structuredContent"]
        assert details["exit_code"] == 0, details
        assert details["stdout"] == "krunlet-mcp-real-vm\n", details
        assert details["containment"] != "none", details

        process.stdin.close()
        assert process.wait(timeout=15) == 0, "MCP did not exit cleanly"
        assert process.stdout.read() == "", "stdout contains non-JSON-RPC data"
        print("MCP stdio real microVM smoke test passed")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=10)
        stderr = process.stderr.read()
        if stderr:
            print(stderr, file=sys.stderr)


if __name__ == "__main__":
    main()
