# ReCasaOS-MCP

An optional module of [ReCasaOS](https://github.com/ReCasaOS): it lets an AI assistant you
choose read and manage the box, with a token you create. Not released yet: this is the
skeleton of the service, with its status route.

`min_recasaos` in `module.json` is a placeholder (`0.5.999`). The real value is the ReCasaOS
release that carries the changes this module needs; set it before the first tag.

## The routes

Under `/v2/mcp`: `GET /status` for the administrator, described in `api/mcp/openapi.yaml`.
`/server` is the MCP endpoint itself; it is not a REST route and is not in the OpenAPI file.

## Tests

    go generate ./...
    go test ./...

## What has been seen working

First proof on a real stack, 2026-11-24, in a Linux container: the real Gateway, accounts
service, message bus and core, with this module built from `bee7b42`.

Seen:

- The tests of the module, of the accounts service's internal route and of the Gateway's
  route table pass on Linux with the race detector; the snapshot release builds both archives.
- The administrator creates a read token and a full token and revokes one; a revoked token
  gets 401 at once; the list of tokens never shows a token's value.
- Without a token the MCP endpoint answers 401 with `WWW-Authenticate: Bearer`; a foreign
  `Origin` gets 403; after 10 bad tokens from one address the 11th gets 429, and a client
  cannot get round it with `X-Forwarded-For`: the Gateway replaces that header with the
  address of its own client (only one client address was tried).
- The accounts service's internal route is refused to a request from outside the box, even
  one carrying the real internal secret.
- A real MCP client connects over plain HTTP on the LAN with the token in a header, sees only
  `search_actions`, `describe_action` and `read` with a read token, finds and runs a read
  action, and runs a `change` action with a full token once that tool is allowed.
- The journal `/var/log/casaos/mcp-calls.log` (mode 0600) holds one line per tool call and
  no token, parameter value or body; no password, internal secret or token was found in
  the module's logs or in the tool results that were saved.
- The module stops within a tenth of a second on SIGTERM with a listening stream open.

Not seen yet:

- Installing and removing an app, and whether a client asks before `destroy`.
- Secrets of apps, backups and destinations kept out of read results (only the core's and
  the accounts service's actions were in the catalogue).
- App management, storage, virtual machines; the module under systemd as root on a box.
