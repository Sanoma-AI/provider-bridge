# testdata: notes for coding agents

Fixtures for tests and for the Sanoma TypeScript client. See [README.md](README.md) for what each file is and how it was recorded.

## Contents

| Path | What |
| --- | --- |
| `pins.json` | provider releases tests may download, pinned by the sha256 of their SHA256SUMS |
| `schemas/` | schema documents (`formatVersion` 1) per provider release; `hashicorp_null_*` and `sanoma_testprov_*` are goldens rewritten by `-update` |
| `replies/` | recorded Import/Read replies, written only by `cmd/bridge-record` |

## Rules

- Do not hand-edit `schemas/` or `replies/`; re-record with `bridge-record` or regenerate goldens with `-update`.
- A changed pin in `pins.json` means a different release: re-record its schema and replies in the same commit.
- `cmd/bridge-record`'s `TestReplayFixtures` and `TestSchemaFixtures` parse everything here in CI.
