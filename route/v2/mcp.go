package v2

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ReCasaOS/ReCasaOS-MCP/codegen"
	"github.com/ReCasaOS/ReCasaOS-MCP/common"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/labstack/echo/v4"
)

// Endpoint is the path an assistant connects to.
const Endpoint = "/v2/mcp/server"

type MCP struct {
	tokens *service.Store
}

func NewMCP(tokens *service.Store) codegen.ServerInterface {
	return &MCP{tokens: tokens}
}

func message(ctx echo.Context, code int, text string) error {
	return ctx.JSON(code, codegen.BaseResponse{Message: &text})
}

func toAPI(t service.Token) codegen.Token {
	return codegen.Token{Id: t.ID, Name: t.Name, Account: t.Account, Level: codegen.Level(t.Level), Created: t.Created, LastUsed: t.LastUsed}
}

// GetStatus has no actions and nothing unclassified yet: later tasks fill them in.
func (m *MCP) GetStatus(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, codegen.Status{Version: common.Version, Endpoint: Endpoint, Tokens: len(m.tokens.List())})
}

func (m *MCP) ListTokens(ctx echo.Context) error {
	tokens := []codegen.Token{}
	for _, t := range m.tokens.List() {
		tokens = append(tokens, toAPI(t))
	}

	return ctx.JSON(http.StatusOK, tokens)
}

func (m *MCP) CreateToken(ctx echo.Context) error {
	var body codegen.NewToken
	if err := ctx.Bind(&body); err != nil {
		return message(ctx, http.StatusBadRequest, "the request body is not valid")
	}

	// the account is the caller's, as the JWT middleware set it: never a field of the body
	account, err := strconv.Atoi(ctx.Request().Header.Get("user_id"))
	if err != nil {
		return message(ctx, http.StatusForbidden, "a token belongs to an account: sign in to create one")
	}

	level := service.LevelRead
	if body.Level != nil {
		level = service.Level(*body.Level)
	}

	plain, t, err := m.tokens.Create(body.Name, account, level)
	switch {
	case errors.Is(err, service.ErrTooMany):
		return message(ctx, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrBadName), errors.Is(err, service.ErrBadLevel):
		return message(ctx, http.StatusBadRequest, err.Error())
	case err != nil:
		return message(ctx, http.StatusInternalServerError, "the token could not be saved")
	}

	a := toAPI(t)

	return ctx.JSON(http.StatusCreated, codegen.CreatedToken{
		Id: a.Id, Name: a.Name, Account: a.Account, Level: a.Level, Created: a.Created, LastUsed: a.LastUsed, Token: plain,
	})
}

func (m *MCP) DeleteToken(ctx echo.Context, id string) error {
	if _, ok := m.tokens.Delete(id); !ok {
		return message(ctx, http.StatusNotFound, "no such token")
	}

	return ctx.NoContent(http.StatusNoContent)
}
