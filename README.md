# CasaOS-Common

> **Not affiliated with IceWhale.** An independent, community-maintained distribution of CasaOS, not produced or endorsed by Shanghai IceWhale Technology Limited. CASAOS is their trademark, used here only to say what this is a release of. The original project is [IceWhaleTech/CasaOS](https://github.com/IceWhaleTech/CasaOS); report problems with this distribution at [ReCasaOS/CasaOS/issues](https://github.com/ReCasaOS/CasaOS/issues).

Common structs and functions shared by the components of the [ReCasaOS](https://github.com/ReCasaOS/CasaOS-Install#readme): the JWT helpers the services authenticate each other with, the logger, the shared error codes and models, the systemd and command helpers, and the small clients each component uses to reach the others.

## Why this fork exists

Every other component of this distribution was already forked; this library was not. Its six consumers each pinned a different IceWhale alpha — from `v0.4.4-alpha2` to `v0.4.11-alpha4` — of a module nobody here could patch. Nothing was broken: those versions are immutable on `proxy.golang.org` and would keep building even if the upstream repository disappeared. What was missing was the ability to fix a bug in shared authentication code, which is most of what this library contains.

This fork starts at upstream `v0.4.21`, unchanged apart from the module path, which is now `github.com/inkly/CasaOS-Common`. The six components are unified on it, where before each carried its own version.

## Install

Components are not installed individually. The whole distribution is installed and upgraded with one command:

```sh
curl -fsSL https://github.com/ReCasaOS/CasaOS-Install/releases/latest/download/install.sh | sudo bash
```

What a release contains, and how it is built, is described in [CasaOS-Install](https://github.com/ReCasaOS/CasaOS-Install#readme).

## Development

```sh
go build ./...
go test ./...
```

No code generation, no network access at build time, no build tags. The module targets Go 1.21.

## Licence

Apache License 2.0 — see [LICENSE](LICENSE), which is the stock text; the per-file IceWhale copyright notices in the source are kept, as the licence requires.

CasaOS is the work of IceWhale and its contributors. CasaOS is a mark of IceWhale; this distribution uses the name to say what it is a release of, and nothing more. This fork is not affiliated with IceWhale and claims no endorsement by it.
