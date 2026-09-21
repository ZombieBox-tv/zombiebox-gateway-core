#!/usr/bin/env python3
"""Exercise the discovery-only CLI on loopback without opening private state."""

import argparse
import secrets
import socket
import subprocess
import tempfile
import time
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    with tempfile.TemporaryDirectory(prefix="zombie-discovery-") as directory:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        process = subprocess.Popen(
            [str(binary), "-discovery-only", "-discovery-listen", f"127.0.0.1:{port}"],
            cwd=directory,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
                client.settimeout(0.2)
                nonce = secrets.token_hex(16)
                request = f"ZOMBIE_DISCOVER_V1 {nonce}\n".encode()
                deadline = time.monotonic() + 5
                while True:
                    if process.poll() is not None:
                        raise RuntimeError("discovery process exited before replying")
                    client.sendto(request, ("127.0.0.1", port))
                    try:
                        response, source = client.recvfrom(128)
                        assert source == ("127.0.0.1", port)
                        assert response == f"ZOMBIE_GATEWAY_V1 {nonce} 8090\n".encode()
                        break
                    except TimeoutError:
                        if time.monotonic() >= deadline:
                            raise RuntimeError(
                                "discovery response deadline exceeded"
                            ) from None
            assert not list(Path(directory).iterdir()), "discovery opened private state"
        finally:
            process.terminate()
            try:
                process.communicate(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate()
                raise RuntimeError("discovery ignored shutdown") from None
        assert process.returncode == 0, "unclean discovery shutdown"
    print(
        "PASS: discovery-only startup, nonce/locator, no state files and graceful shutdown"
    )


if __name__ == "__main__":
    main()
