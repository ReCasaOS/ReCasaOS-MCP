//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/mcp/openapi.yaml > codegen/mcp_api.go"

package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/model"
	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/common"
	"github.com/ReCasaOS/ReCasaOS-MCP/mcpserver"
	"github.com/ReCasaOS/ReCasaOS-MCP/pkg/config"
	"github.com/ReCasaOS/ReCasaOS-MCP/route"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"

	util_http "github.com/ReCasaOS/CasaOS-Common/utils/http"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/mcp/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/mcp.conf.sample
	_confSample string
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// parse arguments and intialize
	{
		configFlag := flag.String("c", "", "config file path")
		versionFlag := flag.Bool("v", false, "version")

		flag.Parse()

		if *versionFlag {
			fmt.Printf("v%s\n", common.Version)
			os.Exit(0)
		}

		println("git commit:", commit)
		println("build date:", date)

		config.InitSetup(*configFlag, _confSample)

		logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)
	}

	// setup listener
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic(err)
	}

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "mcp.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// initialize routers and register at gateway
	{
		gateway, err := external.NewManagementService(config.CommonInfo.RuntimePath)
		if err != nil {
			panic(err)
		}

		for _, apiPath := range []string{route.V2APIPath, route.V2DocPath} {
			if err := gateway.CreateRoute(&model.Route{
				Path:   apiPath,
				Target: "http://" + listener.Addr().String(),
			}); err != nil {
				panic(err)
			}
		}
	}

	if err := os.MkdirAll(config.AppInfo.StatePath, 0o700); err != nil {
		panic(err)
	}

	tokens, err := service.OpenStore(filepath.Join(config.AppInfo.StatePath, "tokens.json"))
	if err != nil {
		panic(err)
	}
	for _, note := range tokens.Notes() {
		logger.Error(note)
	}

	// Nothing calls the box here: the catalogue is first read at the first tool call, with
	// the account of the token that makes it.
	identity := service.NewIdentity(config.CommonInfo.RuntimePath)
	box := service.NewBox(config.CommonInfo.RuntimePath, service.DefaultGatewayINI, identity)
	actions := service.NewCatalogue(box)
	journal := service.NewJournal(filepath.Join(config.AppInfo.LogPath, "mcp-calls.log"))
	defer journal.Close()

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v2": route.InitV2Router(route.Deps{
				Server:    mcpserver.Handler(mcpserver.Deps{Tokens: tokens, Catalogue: actions, Box: box, Journal: journal, Note: noteRefusal}),
				Tokens:    tokens,
				Catalogue: actions,
				Identity:  identity,
			}),
			"doc": route.InitV2DocRouter(_docHTML, _docYAML),
		},
	}

	// notify systemd that we are ready
	{
		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that MCP service is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that MCP service is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}

		logger.Info("MCP service is listening...", zap.Any("address", listener.Addr().String()))
	}

	s := httpServer(ctx, mux)

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("error when shutting down the http server", zap.Error(err))
		}
	}()

	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

// httpServer serves h with request contexts that end with ctx, so that a request held open,
// such as an AI client's subscriptions/listen, does not hold up the shutdown.
func httpServer(ctx context.Context, h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

func noteRefusal(msg, addr string, count int) {
	logger.Warn(msg, zap.String("address", addr), zap.Int("count", count))
}
