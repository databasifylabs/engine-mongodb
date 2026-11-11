# Databasify MongoDB Engine

- [Introduction](#introduction)
- [Lines](#lines)
- [Features](#features)
- [Ports](#ports)
- [Engine Notes](#engine-notes)
- [Running Locally](#running-locally)
- [Adding a Version Line](#adding-a-version-line)

## Introduction

The MongoDB engine for [Databasify](https://github.com/databasifylabs/databasify), built on the [engine SDK](https://github.com/databasifylabs/engine-sdk).

Each image is the official `mongo` image plus `databasify`, the engine binary that runs as PID 1 and answers the SDK's typed commands. Images are published as `ghcr.io/databasifylabs/databasify-mongodb`.

The commands and the flows Databasify runs are the same for every engine; see the SDK's [Flows](https://github.com/databasifylabs/engine-sdk#flows).

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
| Log forwarding and host tuning | ✓ |

A command the engine doesn't offer answers `unsupported`; `engine capabilities` lists the ones it does.

## Ports

| Port | Use |
| --- | --- |
| 25060 | `client`, for applications and the other members |

## Engine Notes

- The engine builds on [engine-mysql-family](https://github.com/databasifylabs/engine-mysql-family) for its shared helpers and its migration runner.
- A `SCRAM-SHA-256` password can't contain control characters such as a newline; `user create` and `user set-password` refuse it with `invalid_request`.
- `restore start` refuses a restore into a node without its own `archive`.
- Percona Backup for MongoDB doesn't support 9.0 yet, so Databasify offers no point-in-time recovery on that line.

## Running Locally

`$IMAGES` is `tools/images.sh` in a clone of [engine-sdk](https://github.com/databasifylabs/engine-sdk) at the version `go.mod` requires. Run it from this repository's root:

```bash
go vet ./... && go test -race -count=1 ./...
$IMAGES build mongodb-9.0 --load --tag smoke:mongodb-9.0
$IMAGES smoke smoke:mongodb-9.0 mongodb
```

The smoke test starts the image, runs `images/mongodb/smoke/check.sh` and then the SDK's conformance suite.

## Adding a Version Line

Follow `.claude/skills/adding-a-version-line/SKILL.md`. It covers the `versions.json` entry, the code that differs per line in this engine, and the checks to run.
