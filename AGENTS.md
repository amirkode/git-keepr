# AGENTS.md

## Mission
Build and maintain git-keepr: a minimal, reliable, self-hosted Go Git-mirroring and backup utility.

## Invariants
- The product preserves full **reachable Git history**, not source ZIPs or working-tree snapshots.
- Configured GitHub repositories only; read-only fine-grained PATs from environment variables.
- The runtime uses an embedded scheduler; no cron, database, web API, or dashboard.
- Local bare mirrors + optional current full Git bundle in S3-compatible storage.
- Never leak credentials into logs, Git remotes, config, artifacts, or command lines.
- Never delete a successful local mirror or last completed S3 backup because a sync failed.
- Do not imply all force-pushed-away commits, LFS payloads, or GitHub metadata are backed up.

## Engineering workflow
- Make small, testable packages and avoid unnecessary dependencies.
- Use context-aware subprocesses, bounded concurrency, atomic state writes, and process locks.
- Add tests for failures and restore paths, not only happy paths.
- Run `go test ./...`, `go vet ./...`, formatting checks, and a real offline-restore integration test.
- Update README and config examples when behavior changes.
- Do not introduce scope beyond the current release without an explicit requirement.

## Definition of done
Both Compose and systemd installations work, local + S3 restore is verified,
CI builds images and binaries, no real secrets are committed, and limitations are documented.
