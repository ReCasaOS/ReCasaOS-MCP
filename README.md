# ReCasaOS-MCP

An optional module of [ReCasaOS](https://github.com/ReCasaOS) that lets an AI assistant you
choose read and manage your box.

It puts an [MCP](https://modelcontextprotocol.io) endpoint behind the box's Gateway. An
assistant running on a computer of the same network connects to it with a token you create
in the dashboard. Nothing runs on the box but this endpoint: no model, no key, no account
with anyone. The box only answers the assistant you plugged in.

Once it is connected you can ask things like "which of my apps have an update waiting?",
"start a backup of Nextcloud to the NAS", "why is Immich unhealthy?", "stop the Windows
virtual machine", "how loaded is the box right now?" or "is an update of ReCasaOS waiting?".

## Install

On a box running ReCasaOS v0.5.192 or later:

- in the dashboard, **Modules**, then *AI access (MCP)*, **Install**; or
- on the box, `sudo casaos module install mcp`.

It needs no package and takes about 14 MB. `sudo casaos module remove mcp` takes it away;
the tokens stay in `/var/lib/casaos-mcp/` until you delete that folder.

The module is updated with ReCasaOS, or from the **Modules** page when it shows an update.
The tokens are kept across an update.

| Module | Comes with ReCasaOS | What it added |
|---|---|---|
| v0.1.0 | v0.5.192 | the apps, the backups, the virtual machines, merged storage, the vulnerability scan |
| v0.2.0 | v0.5.193 | the system: its state and load, updates, modules, alerts, the upgrade of the operating system, restart and shutdown |

The **Modules** page shows the version that is installed, and the module gives it to an
assistant when it connects.

## Create a token

The owner's dashboard gains an **AI access** card. *New token* asks for a name and a level:

| Level | What the assistant can do |
|---|---|
| **Read only** (the default) | Look: the apps and their state, the backups and their history, the virtual machines, statistics, health. Nothing that can carry a secret, nothing that changes the box. |
| **Everything** | Also read what can carry a secret (an app's compose file, a destination's settings), change things (install, start, stop, back up, configure) and delete things. |

A token with *Everything* has the owner's full power over the box. A stolen token, or an
assistant misled by a booby-trapped text (a web page, a log line, an app's name), can erase
data. Give it only to an assistant you trust with that.

The token is shown once, with the command to give the assistant. The card lists the tokens
with their last use, and revokes one at any time: a revoked token is refused at the very
next call.

## Connect an assistant

The card gives the command ready to paste, with the box's address on your network. For
Claude Code:

    claude mcp add --transport http recasaos http://<box>/v2/mcp/server --header "Authorization: Bearer <token>"

For another assistant that speaks MCP over HTTP, give it the URL `http://<box>/v2/mcp/server`
and the header `Authorization: Bearer <token>`.

The assistant must run on a computer that reaches the box on your local network (or through
your own VPN). Assistants that connect from their vendor's cloud cannot reach it.

## What the assistant gets

Five tools, whatever the number of actions behind them:

| Tool | Does | Level |
|---|---|---|
| `search_actions` | finds actions of the box by words | both |
| `describe_action` | says how to call one: its parameters, its body | both |
| `read` | runs an action that only looks | both |
| `change` | runs an action that alters the box | Everything |
| `destroy` | runs an action that can lose data | Everything |

The assistant names an action and its parameters, never a URL: the module builds the call
from the box's own API descriptions. `destroy` is a tool of its own so that an assistant can
ask you before using it. Whether it does depends on the assistant and on how you set it up.

Every call is made as the owner's account, through the Gateway, like a call of the
dashboard: the box's own rules judge it.

## What is offered, and what is not

Each action of the box's API carries a class in [`catalogue/classes.yaml`](catalogue/classes.yaml),
written after reading what the action does and what it answers. **An action without a class
is not offered at all.** Today 243 actions are classed:

| Class | Count | Meaning | Who gets it |
|---|---|---|---|
| `read` | 75 | only looks, and answers with nothing private | every token |
| `secret` | 34 | reads something that can carry a secret | Everything |
| `change` | 70 | alters the box | Everything |
| `danger` | 43 | can lose data or lock someone out | Everything, through `destroy` |
| `never` | 21 | not offered to any assistant | nobody |

- **Offered:** the apps and their store, updates, logs and statistics; the backups, their
  schedules and destinations; the virtual machines; merged storage and mounts; the
  vulnerability scan; the system (its state and load, the updates of ReCasaOS, of the
  packages and of Docker, the optional modules, the alerts and statistics settings, the
  guided upgrade of the operating system, restart and shutdown).
  Shutdown, restart, every update, the install and removal of a module and each step of
  the operating system's upgrade that acts are `danger`: they go through `destroy`, and a
  *Read only* token neither finds nor runs them.
- **Not offered yet:** the drives, the shares, the accounts,
  Files and the fleet (154 routes, listed in [`catalogue/uncovered.txt`](catalogue/uncovered.txt)).
  They come area by area.
- **Never offered:** passwords, two-factor settings, the creation or deletion of accounts,
  cloud sign-ins, uploads and downloads of files, live streams, consoles and terminals, the SSH sign-in, and
  the module's own tokens.

## What is kept

- `/var/log/casaos/mcp-calls.log`: one line per call an assistant made: the time, the token,
  the tool, the action and the result. Never a parameter, a body or a token.
- `/var/lib/casaos-mcp/tokens.json`: the tokens, as hashes. A token's value exists only on
  the screen where it was created and wherever you pasted it.
- Bad tokens presented to the endpoint are noted in the module's log, with the address they
  came from.

## Limits

- The same network only: no access through ReCasaOS's remote access yet.
- A Gateway whose main port serves HTTPS only, or that listens on one particular address, is
  not supported yet.
- Sending and fetching files, live streams and consoles are not offered.
- A box with several networks gives the address of its main one in the command.
- The box's own list of actions is loaded when an assistant first connects, so the card's
  count is incomplete before that.
- An action the box is too old to have answers with the box's own "not found".

## Development

The module is a Go service, `casaos-mcp`, built like the other ReCasaOS modules: it listens
on the loopback, registers `/v2/mcp` with the Gateway, and ships as two archives
(amd64, arm64) that hold the binary, its unit and a sample configuration.

    go generate ./...
    go test ./...

Routes, under `/v2/mcp`:

- `POST /server`: the MCP endpoint (bearer token; stateless, JSON answers).
- For the owner, described in [`api/mcp/openapi.yaml`](api/mcp/openapi.yaml): `GET /status`,
  `GET` and `POST /tokens`, `DELETE /tokens/{id}`.

When the box's API changes:

    scripts/refresh-testdata.sh <folder that holds the ReCasaOS repositories>

refreshes the copies of the API descriptions under `testdata/`. The tests then fail for
every new action until it has a line in `catalogue/classes.yaml`, and for every route that
is neither described nor listed in `catalogue/uncovered.txt`. The rules for choosing a class
are at the top of the classes file; `TestClassCounts` holds the totals, so that a change of
class is always a visible edit.

The box serves no description of its older `/v1` routes. The module carries hand-written
ones, one file per area under [`catalogue/v1/`](catalogue/v1/) (OpenAPI, written from the
handlers: what each one reads, and a sentence an assistant can search). They join the
catalogue beside the box's own and follow the same rule: no class, not offered. The tests
hold each of them to the list of routes, refuse one that the box also describes, and
[`catalogue/v1/not-in-routes.txt`](catalogue/v1/not-in-routes.txt) names those newer than
that list. An assistant finds an action by the words of its summary, its path and its
name: put there the words a person would type.

A release is a tag `v<number>.<number>.<number>` on `main`, after `Version` in
`common/constants.go` has been set to the same number; the installer's catalogue then pins
that tag and its commit.

## Tested

- The unit tests run on Linux with the race detector, on every pull request.
- On a full stack in a container (the real Gateway, accounts service, message bus and core)
  with a real MCP client: tokens created and revoked, the three reading tools alone with a
  read token, a read and a change run, a revoked token refused at once, the journal free of
  any value.
- For the system's actions (v0.2.0), on the same stack: a read token reads the load and the
  update channel and neither finds nor can call shutdown, the updates or the upgrade plan; a
  full token changes the update channel and puts it back; thirty system actions called,
  each answered by the box. No action that goes through `destroy` was run.
- On a real box by the maintainer, before the release that first offered the module
  (v0.1.0).

## Licence

Apache 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
