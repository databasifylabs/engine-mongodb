# Databasify MongoDB Engine

- [Introduction](#introduction)
- [Lines](#lines)
- [Features](#features)
- [Ports](#ports)
- [Notes](#notes)
- [Running Locally](#running-locally)
- [Adding a Version Line](#adding-a-version-line)

## Introduction

The MongoDB engine for [Databasify](https://github.com/databasifylabs/databasify), built on the [engine SDK](https://github.com/databasifylabs/engine-sdk).

Each image is the official `mongo` image plus its tools and `databasify`, the [engine SDK](https://github.com/databasifylabs/engine-sdk) 1.0 runtime that runs as PID 1. Images are published as `ghcr.io/databasifylabs/databasify-mongodb`.

Engine behaviour lives in the Databasify app: its PHP driver for this engine runs Bash scripts inside the container through the runtime. This repository changes only when the database or one of its tools moves to a new version.

## Lines

MongoDB 7.0, 8.0 and 9.0. Each line takes patches of its release only and never leaves it. The weekly refresh moves the pins in `versions.json`.

## Features

| Feature | MongoDB |
| --- | --- |
| Node lifecycle, status, metrics, TLS | ✓ |
| Databases and users with read/write grants | ✓ |
| Password encryptions | `SCRAM-SHA-256`, `SCRAM-SHA-1` |
| High availability | replica set |
| Automatic and manual failover, force quorum | ✓ |
| Standbys (up to 2) and read-only nodes (any number) | ✓ (50 members at most) |
| Primary key required in a cluster | — |
| Connection pools | — |
| Backups and point-in-time restore | Percona Backup for MongoDB |
| Continuous migration | Percona ClusterSync, same major version |
| Fork (one-shot copy) | `mongodump` and `mongorestore` |
| Query insights | database profiler |
| Host tuning | ✓ |

## Ports

| Port | Use |
| --- | --- |
| 25060 | `client`, for applications and the other members |

## Notes

- Percona Backup for MongoDB doesn't support 9.0 yet, so Databasify offers no point-in-time recovery on that line.

## Running Locally

`$IMAGES` is `tools/images.sh` in a clone of [engine-sdk](https://github.com/databasifylabs/engine-sdk) at the tag the `sdk` pin in `versions.json` names (`v1.0.0`). Run it from this repository's root:

```bash
$IMAGES check
$IMAGES build mongodb-9.0 --load --tag smoke:mongodb-9.0
$IMAGES smoke smoke:mongodb-9.0 mongodb
```

The smoke test starts the image, runs `images/mongodb/smoke/check.sh` and then the SDK's runtime conformance suite.

## Adding a Version Line

Follow `.claude/skills/adding-a-version-line/SKILL.md`. It covers the `versions.json` entry, what differs per line, and the checks to run.
