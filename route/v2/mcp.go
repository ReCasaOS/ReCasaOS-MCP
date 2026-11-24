package v2

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	"github.com/ReCasaOS/ReCasaOS-MCP/codegen"
	"github.com/ReCasaOS/ReCasaOS-MCP/common"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// Endpoint is the path an assistant connects to.
const Endpoint = "/v2/mcp/server"

type MCP struct {
	tokens    *service.Store
	catalogue *service.Catalogue
	identity  *service.Identity
}

func NewMCP(tokens *service.Store, catalogue *service.Catalogue, identity *service.Identity) codegen.ServerInterface {
	return &MCP{tokens: tokens, catalogue: catalogue, identity: identity}
}

func message(ctx echo.Context, code int, text string) error {
	return ctx.JSON(code, codegen.BaseResponse{Message: &text})
}

func toAPI(t service.Token) codegen.Token {
	return codegen.Token{Id: t.ID, Name: t.Name, Account: t.Account, Level: codegen.Level(t.Level), Created: t.Created, LastUsed: t.LastUsed}
}

// GetStatus counts the catalogue as last loaded: it is empty until the first tool call.
func (m *MCP) GetStatus(ctx echo.Context) error {
	byClass, unclassified := m.catalogue.Counts()
	return ctx.JSON(http.StatusOK, codegen.Status{
		Version:  common.Version,
		Endpoint: Endpoint,
		Tokens:   len(m.tokens.List()),
		Actions: codegen.ActionCounts{
			Read: byClass[catalogue.Read], Secret: byClass[catalogue.Secret], Change: byClass[catalogue.Change], Danger: byClass[catalogue.Danger],
		},
		Unclassified: unclassified,
	})
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
	t, err := m.tokens.Delete(id)
	switch {
	case errors.Is(err, service.ErrNotFound):
		return message(ctx, http.StatusNotFound, err.Error())
	case err != nil:
		logger.Error("a token could not be revoked: the file cannot be written", zap.String("id", id), zap.Error(err))
		return message(ctx, http.StatusInternalServerError, "the revocation could not be saved: the token still works")
	}

	// the account's access token outlives no token of its own
	if !m.tokens.AccountHasTokens(t.Account) {
		m.identity.Forget(t.Account)
	}

	return ctx.NoContent(http.StatusNoContent)
}
