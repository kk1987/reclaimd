# Security

This is a one-person project, so there is no SLA. I will look at anything sent
in and fix what is real. I hope to do that within a week, but cannot promise it.

## Reporting

Use GitHub's private advisory form:
https://github.com/kk1987/reclaimd/security/advisories/new. Please do not open a
public issue for anything that lets someone reach a disk they should not.

## What I consider a vulnerability

`reclaimd refresh` writes to block devices, and the daemon runs where it can
open them. So: anything that makes the daemon itself write to a disk, anything
that gets `refresh` past its four gates without the serial printed on the
device, anything that turns the status server into a way to reach a device or a
file it should not, and any path that writes back a block that failed to read.
Those blocks are meant to be left alone.

## The status server

It binds `127.0.0.1:8099` by default. A `ui_token` turns on bearer auth on every
route, and the two POST routes (`enabled` and `scan`) additionally require a
same-origin `Sec-Fetch-Site` and a JSON content type. There is no `refresh` over
HTTP at all.

The page does assemble the `refresh` command line for the disk on screen, serial
and all, for somebody to copy into a shell. That runs nothing by itself. The
daemon still holds every device read-only, and its unit gives the kernel
`block-sd r` to enforce it. But it does mean anyone who can read the status page
can read the serial that gate 1 asks for, so treat read access to the page with
the same care as the drive it describes. That is the one respect in which a
LAN-exposed page is worse than it was before the panel existed.

Binding off-loopback with no token logs a warning and starts anyway. So a status
page readable by the whole LAN is reachable by editing one config field, and I
would rather hear that argued as a bug than not. Bypassing the token, or getting
a POST through from another origin, is straightforwardly one.

## Versions

Only the latest release. There are no maintenance branches.
