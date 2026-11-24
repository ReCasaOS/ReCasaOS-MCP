# ReCasaOS-MCP

An optional module of [ReCasaOS](https://github.com/ReCasaOS) that puts an MCP endpoint
behind the box's Gateway. An AI assistant connects with a token the owner creates, at one of
two levels, `read` or `full`. Every call is made as the owner's account, through the Gateway.
Each action of the box's API carries a class in `catalogue/classes.yaml`; an action without a
class is not offered.

## Status

Not released, and not in the installer's catalogue. `min_recasaos` in `module.json` is a
placeholder (`0.5.999`) until the ReCasaOS release that carries the two changes this module
needs in the Gateway and in the accounts service.

## Routes

Under `/v2/mcp`:

- `POST /server`: the MCP endpoint, with a bearer token. It is not a REST route and is not
  in the OpenAPI file.
- The owner's routes, described in `api/mcp/openapi.yaml`: `GET /status` (it also gives the endpoint's address on the local network),
  `GET|POST /tokens`, `DELETE /tokens/{id}`.

## Connecting an assistant

    claude mcp add --transport http <name> http://<box>/v2/mcp/server --header "Authorization: Bearer <token>"

It works for an assistant running on a computer of the same network.

## The tools

- `search_actions`: find an action of the box.
- `describe_action`: how to call it.
- `read`: run an action that only looks.
- `change`: run an action that alters the box (full token only).
- `destroy`: run an action that can lose data (full token only).

## Files on the box

- `/var/lib/casaos-mcp/tokens.json`: the tokens, as hashes only.
- `/var/log/casaos/mcp-calls.log`: one line per tool call: time, token id and name, tool,
  action, status. Never a value.
- The module's general log, with the other ReCasaOS services.

## Limits

- Same network only.
- A Gateway that serves HTTPS only on its main port, or that is bound to an address other
  than the loopback or all addresses, is not supported yet.
- Uploads, downloads, streams and consoles are not offered.
- The list of actions is loaded at the first tool call, so the status route shows no action
  before that.
- Whether the assistant asks before a change or a deletion depends on the assistant.

## Tests

    go generate ./...
    go test ./...

To refresh `testdata/` (the box's API descriptions and route list):

    scripts/refresh-testdata.sh <workspace root>

## What has been seen working

First proof on a real stack, 2026-11-24, in a Linux container: the real Gateway, accounts
service, message bus and core, with this module built from `bee7b42`.

Seen:

- The tests of the module, of the accounts service's internal route and of the Gateway's
  route table pass on Linux with the race detector; the snapshot release builds both archives.
- The administrator creates a read token and a full token and revokes one; a revoked token
  gets 401 at once; the list of tokens never shows a token's value.
- Without a token the MCP endpoint answers 401 with `WWW-Authenticate: Bearer`; a foreign
  `Origin` gets 403; after 10 bad tokens from one address the 11th gets 429 (only one
  client address was tried).
- The accounts service's internal route refuses a request made from another machine of the network.
- A real MCP client connects over plain HTTP on the LAN with the token in a header, sees only
  `search_actions`, `describe_action` and `read` with a read token, finds and runs a read
  action, and runs a `change` action with a full token once that tool is allowed.
- The journal `/var/log/casaos/mcp-calls.log` (mode 0600) holds one line per tool call and
  no token, parameter value or body; no password, internal secret or token was found in
  the module's logs or in the tool results that were saved.
- The module stops within a tenth of a second on SIGTERM with a listening stream open.

Not seen yet:

- Installing and removing an app, and whether a client asks before `destroy`.
- The actions of app management, storage and virtual machines (only the core's and the
  accounts service's actions were in the catalogue); the module under systemd on a box.
- The token guard as it is now (a valid token always passes; bad ones are counted per
  address) was tested on Linux with the race detector, not on the stack.
