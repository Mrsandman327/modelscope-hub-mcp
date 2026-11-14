package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "modelscope-hub-mcp"
	serverVersion = "v1.0.0"
)

func main() {
	transport := flag.String("transport", "stdio", "传输方式：stdio 或 http")
	addr := flag.String("http-addr", "127.0.0.1:8080", "网络传输监听地址，默认仅允许本机连接")
	flag.Parse()
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	c := newHubClient(cfg)
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)
	registerTools(server, c)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	switch *transport {
	case "stdio":
		log.Printf("%s 已启动，使用标准输入输出传输", serverName)
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(c.redact(err.Error()))
		}
	case "http":
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		httpServer := &http.Server{
			Addr: *addr, Handler: handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
		log.Printf("%s 已启动，监听 %s；请勿无鉴权暴露到公网", serverName, *addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(c.redact(err.Error()))
		}
	default:
		log.Fatal("传输方式只能选择 stdio 或 http")
	}
}
