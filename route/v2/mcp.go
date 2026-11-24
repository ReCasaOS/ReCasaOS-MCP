package v2

import (
	"net/http"

	"github.com/ReCasaOS/ReCasaOS-MCP/codegen"
	"github.com/ReCasaOS/ReCasaOS-MCP/common"
	"github.com/labstack/echo/v4"
)

// Endpoint is the path an assistant connects to.
const Endpoint = "/v2/mcp/server"

type MCP struct{}

func NewMCP() codegen.ServerInterface {
	return &MCP{}
}

// GetStatus has no actions, no tokens and nothing unclassified yet: later tasks fill them in.
func (m *MCP) GetStatus(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, codegen.Status{Version: common.Version, Endpoint: Endpoint})
}
