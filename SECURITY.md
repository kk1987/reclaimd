# Security

This is a one-person project, so there is no SLA. I will look at anything sent
in and fix what is real, but "within a week" is a hope, not a promise.

## Reporting

Use GitHub's private advisory form:
https://github.com/kk1987/reclaimd/security/advisories/new. Please do not open a
public issue for anything that lets someone reach a disk they should not.

## What I consider a vulnerability

`reclaimd refresh` writes to block devices, and the daemon runs where it can
open them. So: anything that makes the daemon itself write to a disk, anything
that gets `refresh` past its four gates without the serial printed on the
device, anything that turns the status server into a way to reach a device or a
file it should not, and any path where a block that failed to read gets written
back rather than left alone.

## The status server

It binds `127.0.0.1:8099` by default. A `ui_token` turns on bearer auth on every
route, and the two POST routes (`enabled` and `scan`) additionally require a
same-origin `Sec-Fetch-Site` and a JSON content type. There is no `refresh` over
HTTP at all.

Binding off-loopback with no token only logs a warning — it does not refuse to
start. So a status page readable by the whole LAN is reachable by editing one
config field, and I would rather hear that argued as a bug than not. Bypassing
the token, or getting a POST through from another origin, is straightforwardly
one.

## Versions

Only the latest release. There are no maintenance branches.
