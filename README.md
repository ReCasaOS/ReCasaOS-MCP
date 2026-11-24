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
