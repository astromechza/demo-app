package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func main() {
	if err := mainInner(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

const DefaultListenAddr = ":8080"
const DefaultColor = "random"

type Globals struct {
	BackgroundColor string
	Motd            string
	Hostname        string
	Pid             int
	StartedAt       string
	ProxyTo         string
	Environment     []string

	requestCount  uint64
	RuntimeExtras map[string]interface{}
	Interfaces    map[string]string
	Args          string
	Uid           int
	Gid           int
}

func (g *Globals) GetRequestCount() uint64 {
	return atomic.LoadUint64(&g.requestCount)
}

var DefaultGlobals Globals

func mainInner() error {
	listenAddress := flag.String("listen", DefaultListenAddr, "the address to listen on")
	backgroundColor := flag.String("color", DefaultColor, "the background color to display")
	proxyTo := flag.String("proxy", "", "forward the request to the given http or https endpoint")
	motdString := flag.String("motd", "Hello World", "specify a message of the day, prefix with '@' to read from a file")

	flag.Parse()
	if flag.NArg() > 0 {
		return errors.New("no positional arguments allowed")
	}
	var visitError error
	flag.VisitAll(func(f *flag.Flag) {
		envName := "OVERRIDE_" + strings.ToUpper(f.Name)
		if v := os.Getenv(envName); v != "" {
			visitError = errors.Wrap(f.Value.Set(v), "failed to set '"+f.Name+"'")
		}
	})
	if visitError != nil {
		return visitError
	}

	if *backgroundColor == "random" {
		colorBytes := []byte{0, 0, 0}
		for i := range colorBytes {
			colorBytes[i] = byte((rand.Intn(256) + 255) / 2)
		}
		*backgroundColor = fmt.Sprintf("#%x", colorBytes)
	}

	if strings.HasPrefix(*motdString, "@") {
		content, err := os.ReadFile(strings.TrimPrefix(*motdString, "@"))
		if err != nil {
			slog.Error("failed to read motd file", "err", err)
		} else {
			*motdString = string(content)
		}
	}

	DefaultGlobals.Motd = *motdString
	DefaultGlobals.Pid = os.Getpid()
	DefaultGlobals.Args = strings.Join(os.Args, " ")
	DefaultGlobals.BackgroundColor = *backgroundColor
	DefaultGlobals.StartedAt = time.Now().Format(time.RFC3339)
	DefaultGlobals.Environment = os.Environ()
	DefaultGlobals.Uid = os.Geteuid()
	DefaultGlobals.Gid = os.Getgid()

	sort.Strings(DefaultGlobals.Environment)
	DefaultGlobals.RuntimeExtras = map[string]interface{}{
		"GOOS":      runtime.GOOS,
		"GOARCH":    runtime.GOARCH,
		"GOVERSION": runtime.Version(),
		"NumCPU":    runtime.NumCPU(),
	}

	{
		ifaces, _ := net.Interfaces()
		DefaultGlobals.Interfaces = make(map[string]string, len(ifaces))
		for _, iface := range ifaces {
			rawAddresses, _ := iface.Addrs()
			addresses := make([]string, len(rawAddresses))
			for i, address := range rawAddresses {
				addresses[i] = address.String()
			}
			if len(addresses) > 0 {
				DefaultGlobals.Interfaces[iface.Name] = fmt.Sprintf("%s mac:%s", strings.Join(addresses, " ,"), iface.HardwareAddr)
			}
		}
	}
	{
		var stat unix.Statfs_t
		if unix.Statfs("/", &stat) == nil {
			DefaultGlobals.RuntimeExtras["RootFsSize"] = stat.Blocks * uint64(stat.Bsize)
		}
		if unix.Statfs(os.TempDir(), &stat) == nil {
			DefaultGlobals.RuntimeExtras["TempFsSize"] = stat.Blocks * uint64(stat.Bsize)
		}
	}

	if proxyTo != nil && *proxyTo != "" {
		if v, err := url.Parse(*proxyTo); err != nil {
			return errors.Wrap(err, "invalid proxy flag")
		} else if v.Scheme != "http" && v.Scheme != "https" {
			return errors.New("invalid scheme")
		}
	}

	if v, err := os.Hostname(); err != nil {
		DefaultGlobals.Hostname = "failed to get hostname"
	} else {
		DefaultGlobals.Hostname = v
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return errors.Wrap(err, "failed to listen")
	}
	slog.Info("Listening", "address", listener.Addr().Network()+"://"+listener.Addr().String())

	echoServer := echo.New()
	echoServer.HideBanner = true
	echoServer.HidePort = true

	// configure response timeout first
	echoServer.Use(middleware.ContextTimeout(time.Second * 10))

	// now the logger
	echoServer.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			elapsed := time.Since(start)
			if err != nil {
				echoServer.HTTPErrorHandler(err, c)
			}
			slog.Info(
				"Handled",
				"method", c.Request().Method, "url", c.Request().URL, "status", c.Response().Status,
				"elapsed", elapsed, "resp_size", c.Response().Size, "req_id", c.Response().Header().Get("X-Request-ID"),
			)
			return nil
		}
	})

	// recover from panics
	echoServer.Use(middleware.Recover())

	// assign a request id
	echoServer.Use(middleware.RequestID())

	echoServer.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			atomic.AddUint64(&DefaultGlobals.requestCount, 1)
			return next(c)
		}
	})

	// now the body limiter
	echoServer.Use(middleware.BodyLimit("1M"))

	if *proxyTo != "" {
		v, _ := url.Parse(*proxyTo)
		echoServer.Use(middleware.ProxyWithConfig(middleware.ProxyConfig{
			Skipper: func(c echo.Context) bool {
				if v := c.Response().Header().Get("X-Request-ID"); v != "" {
					c.Request().Header.Set("X-Request-ID", v)
				}
				return len(strings.Split(c.Request().Header.Get("X-Forwarded-For"), ",")) > 4
			},
			Balancer: middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{{
				Name: "next",
				URL:  v,
			}}),
		}))
	}

	// cors and security headers for free
	echoServer.Use(middleware.CORS())
	echoServer.Use(middleware.Secure())
	// rate limit to 100 requests per second
	echoServer.Use(middleware.RateLimiter(middleware.NewRateLimiterMemoryStore(100)))

	// compress response body if acceptable
	echoServer.Use(middleware.Gzip())

	echoServer.GET("/", mainPage)

	go func() {
		exit := make(chan os.Signal, 1)
		signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-exit
		slog.Info("Signal caught", "sig", sig)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		if err := echoServer.Shutdown(ctx); err != nil {
			slog.Error("Failed to close server", "err", err)
		}
	}()

	echoServer.Listener = listener
	if err := echoServer.Start(listener.Addr().String()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

var mainTemplate = template.Must(template.New("root").Parse(`<!DOCTYPE html>
<html lang='en'>
 <head>
  <title>Demo App</title>
  <meta charset='utf-8'>
  <meta http-equiv="refresh" content="5">
  <link rel="icon" href="data:;base64,iVBORw0KGgo=">
 </head>
 <body style="background:{{.Globals.BackgroundColor}}; font-family: monospace, monospace; font-size: 0.8em;">

 <h2>Message of the day: {{.Globals.Motd}}</h3>
  <p>Welcome to Demo App! This is a simple html application for demo's and examples. This can do the following:
  <ul>
	  <li>Show properties of the request connection and headers</li>
	  <li>Show properties of the deployment environment and lifecycle</li>
	  <li>Proxy chain to other demo-app instances up to 5 deep</li>
      <li>Automatically refresh every 5 seconds</li>
  </ul>
  Github Repo: <a target="_blank" href="https://github.com/astromechza/demo-app">https://github.com/astromechza/demo-app</a>.<br /><br />
  To change the message of the day, redeploy with <code>--motd=..</code> or <code>$OVERRIDE_MOTD=..</code> or to change the background color, redeploy with <code>--color=..</code> or <code>$OVERRIDE_COLOR=..</code> .
  </p>

 <hr>
{{ if .Detail }}
 <a href="/">Hide details</a>

 <h3>Request id:{{ .RequestId }} at:{{ .RenderedAt }}</h3>
 <pre>{{ .Request }}</pre>

 <hr>
 <h3>Server hostname:{{ .Globals.Hostname }} pid:{{ .Globals.Pid }}</h3>
 <table>
 <tr><td>Args:</td><td>{{ .Globals.Args }}</td></tr>
 <tr><td>Uid/Gid:</td><td>{{ .Globals.Uid }}/{{ .Globals.Gid }}</td></td>
 <tr><td>Started:</td><td>{{ .Globals.StartedAt }}</td></tr>
 <tr><td>Responses:</td><td>{{ .Globals.GetRequestCount }}</td></tr>
 {{range $k, $v := .Globals.RuntimeExtras }}
 <tr><td>{{ $k }}:</td><td>{{ $v }}</td></tr>
 {{end}}
 </table>

 <h4>Environment</h4>
 <table>
 {{range $val := .Globals.Environment }}
  <tr><td><code>{{ $val }}</code></td></tr>
 {{end}}
 </table>

 <h4>Interfaces</h4>

 <table>
 {{range $k, $v := .Globals.Interfaces }}
 <tr><td>{{ $k }}:</td><td>{{ $v }}</td></tr>
 {{end}}
 </table>
{{ else }}
  <a href="/?detail=true">Show details</a>
{{ end }}
 </body>
</html>`))

func mainPage(c echo.Context) error {
	buff := new(bytes.Buffer)
	if err := c.Request().Write(buff); err != nil {
		return errors.Wrap(err, "failed to buffer request")
	}

	renderedAt := time.Now().UTC().Format(time.RFC3339)

	if err := mainTemplate.Execute(c.Response(), map[string]interface{}{
		"Globals":    &DefaultGlobals,
		"RequestId":  c.Response().Header().Get("X-Request-ID"),
		"Request":    buff.String(),
		"RenderedAt": renderedAt,
		"Detail":     c.Request().URL.Query().Get("detail") != "",
	}); err != nil {
		return errors.Wrap(err, "failed to template")
	}
	return nil
}
