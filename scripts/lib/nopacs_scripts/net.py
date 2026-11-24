"""Loopback ports for the servers and peers a proof starts."""

import socket


def free_ports(n):
    """n distinct TCP ports on 127.0.0.1 that were free when asked. All n are
    bound at once before any is released, so they differ from each other; a
    port may still be taken by another process before its user binds it."""
    socks = [socket.socket() for _ in range(n)]
    for s in socks:
        s.bind(("127.0.0.1", 0))
    ports = [s.getsockname()[1] for s in socks]
    for s in socks:
        s.close()
    return ports


def free_port():
    """One port from free_ports."""
    return free_ports(1)[0]
