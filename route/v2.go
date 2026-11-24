package route

import (
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/ReCasaOS/ReCasaOS-MCP/codegen"
	"github.com/ReCasaOS/ReCasaOS-MCP/pkg/config"
	v2Route "github.com/ReCasaOS/ReCasaOS-MCP/route/v2"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

var (
	_swagger *openapi3.T

	V2APIPath string
	V2DocPath string

	// account is a variable so a test can say what an account is without a user-service.
	account = external.Account
)

// accessLogFormat is echo's default access log with the path in place of the request URI, so
// that nothing a caller puts in a query string reaches the journal.
var accessLogFormat = strings.Replace(echo_middleware.DefaultLoggerConfig.Format, `"uri":"${uri}"`, `"path":"${path}"`, 1)

func init() {
	swagger, err := codegen.GetSwagger()
	if err != nil {
		panic(err)
	}

	_swagger = swagger

	u, err := url.Parse(_swagger.Servers[0].URL)
	if err != nil {
		panic(err)
	}

	V2APIPath = strings.TrimRight(u.Path, "/")
	V2DocPath = "/doc" + V2APIPath
}

// Deps is what the routes are built on.
type Deps struct {
	// Server answers on /server, apart from the owner's checks. Nil answers 501.
	Server http.Handler
	// Tokens is where the assistants' tokens live.
	Tokens *service.Store
	// Catalogue is counted by the status.
	Catalogue *service.Catalogue
	// Identity forgets an account's access token when its last token is revoked.
	Identity *service.Identity
	// LANURL gives the endpoint's address on the local network; nil or false: unknown.
	LANURL func() (string, bool)
}

func isInternal(c echo.Context) bool {
	return external.IsInternalRequest(c.RealIP(), c.Request().Header.Get(echo.HeaderAuthorization), config.CommonInfo.RuntimePath)
}

// ownerOnly lets through the box's own services and the administrator: no other account
// manages how assistants are let into the box.
func ownerOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if isInternal(c) {
			return next(c)
		}

		role, gone, known := account(config.CommonInfo.RuntimePath, c.Request().Header.Get("user_id"))
		if !known {
			message := "the account's role cannot be read for now"
			return c.JSON(http.StatusServiceUnavailable, codegen.BaseResponse{Message: &message})
		}
		if role != "admin" || gone {
			message := "only the administrator manages AI access"
			return c.JSON(http.StatusForbidden, codegen.BaseResponse{Message: &message})
		}

		return next(c)
	}
}

// server is the MCP endpoint. It is not a REST route and answers to its own tokens, so it
// stays outside the JWT, owner and OpenAPI checks. GET and DELETE are refused here, whatever
// the handler behind it is.
func server(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet || r.Method == http.MethodDelete:
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
		case handler == nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"message":"the MCP endpoint is not available yet"}`))
		default:
			handler.ServeHTTP(w, r)
		}
	})
}

func InitV2Router(deps Deps) http.Handler {
	e := echo.New()

	e.Use(echo_middleware.Gzip())

	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{Format: accessLogFormat}))

	e.Use(echojwt.WithConfig(echojwt.Config{
		// loopback AND the internal secret: loopback alone is not an identity
		Skipper: isInternal,
		ParseTokenFunc: func(c echo.Context, token string) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			// the header only, the token in it bare or `Bearer <token>`
			func(c echo.Context) ([]string, error) {
				return []string{jwt.TokenFromAuthorization(c.Request().Header.Get(echo.HeaderAuthorization))}, nil
			},
		},
	}))

	e.Use(ownerOnly)

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}))

	codegen.RegisterHandlersWithBaseURL(e, v2Route.NewMCP(deps.Tokens, deps.Catalogue, deps.Identity, deps.LANURL), V2APIPath)

	mcp := server(deps.Server)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V2APIPath+"/server" {
			mcp.ServeHTTP(w, r)
			return
		}

		e.ServeHTTP(w, r)
	})
}

func InitV2DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V2DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V2DocPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
