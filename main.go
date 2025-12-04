package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/elnormous/contenttype"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
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
var RedisClient *redis.Client
var DatabaserImpl Databaser

func mainInner() error {
	listenAddress := flag.String("listen", DefaultListenAddr, "the address to listen on")
	backgroundColor := flag.String("color", DefaultColor, "the background color to display")
	proxyTo := flag.String("proxy", "", "forward the request to the given http or https endpoint")
	motdString := flag.String("motd", "Hello World", "specify a message of the day, prefix with '@' to read from a file")
	redisString := flag.String("redis", "", "Optional redis url 'redis://<user>:<pass>@<host>:<port>'")
	postgresString := flag.String("postgres", "", "Optional postgres url 'postgres://<user>:<pass>@<host>:<port>/<database>'")
	mysqlString := flag.String("mysql", "", "Optional mysql url 'username:password@tcp(host:port)/dbname'")

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

	if redisString != nil && *redisString != "" {
		slog.Info("Parsing redis string", "url", *redisString)
		opt, err := redis.ParseURL(*redisString)
		if err != nil {
			return errors.Wrap(err, "failed to parse redis string")
		}
		RedisClient = redis.NewClient(opt)
	}

	var err error
	if postgresString != nil && *postgresString != "" {
		slog.Info("Parsing postgres string", "url", *postgresString)
		DatabaserImpl, err = NewPostgresDb(*postgresString)
		if err != nil {
			return errors.Wrap(err, "failed to start postgres pool")
		}
	} else if mysqlString != nil && *mysqlString != "" {
		slog.Info("Parsing mysql string", "url", *mysqlString)
		DatabaserImpl, err = NewMysqlDb(*mysqlString)
		if err != nil {
			return errors.Wrap(err, "failed to connect via mysql")
		}
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

	echoServer.GET("/livez", echo.HandlerFunc(func(c echo.Context) error {
		_, err := c.Response().Write([]byte("{}"))
		return err
	}))

	echoServer.GET("/readyz", echo.HandlerFunc(func(c echo.Context) error {
		subCtx, cancel := context.WithTimeout(c.Request().Context(), time.Second*3)
		defer cancel()
		out := make(map[string]any)
		if RedisClient != nil {
			_, err := RedisClient.Get(subCtx, "counter").Result()
			if err != nil {
				if !errors.Is(err, redis.Nil) {
					c.Response().WriteHeader(http.StatusBadGateway)
					return json.NewEncoder(c.Response()).Encode(map[string]any{
						"error": fmt.Sprintf("failed to get redis key: %v", err.Error()),
					})
				}
			}
			out["redis"] = "ok"
		}
		if DatabaserImpl != nil {
			if _, err := DatabaserImpl.Check(c.Request().Context()); err != nil {
				c.Response().WriteHeader(http.StatusBadGateway)
				return json.NewEncoder(c.Response()).Encode(map[string]any{
					"error": fmt.Sprintf("failed to check database: %v", err.Error()),
				})
			}
			out["database"] = "ok"
		}
		return json.NewEncoder(c.Response()).Encode(out)
	}))

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
  <meta http-equiv="refresh" content="{{ .RefreshSeconds }}">
  <link rel="icon" href="data:;base64,iVBORw0KGgo=">
 </head>
 <body style="background:{{.Globals.BackgroundColor}}; font-family: monospace, monospace; font-size: 0.8em;">

 <h2>Message of the day: {{.Globals.Motd}}</h3>
  <p>Welcome to Demo App! This is a simple html application for demo's and examples. This can do the following:
  <ul>
	  <li>Show properties of the request connection and headers</li>
	  <li>Show properties of the deployment environment and lifecycle</li>
	  <li>Proxy chain to other demo-app instances up to 5 deep</li>
      <li>Automatically refresh every 5 (or ?refresh) seconds</li>
  </ul>
  Github Repo: <a target="_blank" href="https://github.com/astromechza/demo-app">https://github.com/astromechza/demo-app</a>.<br /><br />
  To change the message of the day, redeploy with <code>--motd=..</code> or <code>$OVERRIDE_MOTD=..</code> or to change the background color, redeploy with <code>--color=..</code> or <code>$OVERRIDE_COLOR=..</code> .
  An optional <code>--redis</code> or <code>$OVERRIDE_REDIS</code> can provide a redis:// connection string which will be used below to increment a counter on each request.
  An optional <code>--postgres</code> or <code>$OVERRIDE_POSTGRES</code> can Provide a postgres:// connection string which will be used to test a database. 
  OR an optional <code>--mysql</code> or <code>$OVERRIDE_MYSQL</code> can Provide a mysql DSN string which will be used to test a database. 
  </p>

  <p>Redis counter result: <code>{{ .RedisResult }}</code> .</p>
  <p>Database table count result: <code>{{ .DatabaseResult }}</code> .</p>

 <hr>
{{ if .Detail }}
 <a href="./">Hide details</a>

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
  <a href="./?detail=true">Show details</a>
{{ end }}
 </body>
</html>`))

var applicationJsonType = contenttype.NewMediaType(echo.MIMEApplicationJSON)
var textHtmlType = contenttype.NewMediaType(echo.MIMETextHTML)
var acceptableTypes = []contenttype.MediaType{applicationJsonType, textHtmlType}

func mainPage(c echo.Context) error {
	buff := new(bytes.Buffer)
	if err := c.Request().Write(buff); err != nil {
		return errors.Wrap(err, "failed to buffer request")
	}

	renderedAt := time.Now().UTC().Format(time.RFC3339)

	redisResult := "<no redis client configured>"
	if RedisClient != nil {
		subCtx, cancel := context.WithTimeout(c.Request().Context(), time.Second*3)
		defer cancel()
		v, err := RedisClient.Incr(subCtx, "counter").Result()
		if err != nil {
			redisResult = err.Error()
		} else {
			redisResult = strconv.Itoa(int(v))
		}
	}

	var err error
	dbResult := "<no postgres pool configured>"
	if DatabaserImpl != nil {
		subCtx, cancel := context.WithTimeout(c.Request().Context(), time.Second*3)
		defer cancel()
		dbResult, err = DatabaserImpl.Check(subCtx)
		if err != nil {
			dbResult = err.Error()
		}
	}

	var refreshSeconds int
	refreshSeconds, _ = strconv.Atoi(c.Request().URL.Query().Get("refresh"))
	if refreshSeconds <= 0 {
		refreshSeconds = 5
	}

	acceptable, _, _ := contenttype.GetAcceptableMediaType(c.Request(), acceptableTypes)
	if acceptable.Equal(textHtmlType) {
		c.Response().Header().Set("Content-Type", echo.MIMETextHTMLCharsetUTF8)
		if err := mainTemplate.Execute(c.Response(), map[string]interface{}{
			"Globals":        &DefaultGlobals,
			"RequestId":      c.Response().Header().Get("X-Request-ID"),
			"Request":        buff.String(),
			"RenderedAt":     renderedAt,
			"Detail":         c.Request().URL.Query().Get("detail") != "",
			"RedisResult":    redisResult,
			"DatabaseResult": dbResult,
			"RefreshSeconds": refreshSeconds,
		}); err != nil {
			return errors.Wrap(err, "failed to template")
		}
		return nil
	} else if acceptable.Equal(applicationJsonType) {
		c.Response().Header().Set("Content-Type", echo.MIMEApplicationJSON)
		e := json.NewEncoder(c.Response())
		e.SetIndent("", "  ")
		if err := e.Encode(map[string]interface{}{
			"RequestId":      c.Response().Header().Get("X-Request-ID"),
			"Globals":        &DefaultGlobals,
			"RawRequest":     buff.String(),
			"RedisResult":    redisResult,
			"DatabaseResult": dbResult,
		}); err != nil {
			return errors.Wrap(err, "failed to template")
		}
		return nil
	} else {
		return c.NoContent(http.StatusNotAcceptable)
	}
}

type Databaser interface {
	Check(ctx context.Context) (string, error)
}

type PostgresDb struct {
	Pool *pgxpool.Pool
}

func NewPostgresDb(cs string) (*PostgresDb, error) {
	dbPool, err := pgxpool.New(context.Background(), cs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start postgres pool")
	}
	return &PostgresDb{Pool: dbPool}, nil
}

func (p *PostgresDb) Check(ctx context.Context) (string, error) {
	subCtx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	var output int
	if err := p.Pool.QueryRow(subCtx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema');
`).Scan(&output); err != nil {
		return "", err
	} else {
		return strconv.Itoa(output), nil
	}
}

type MysqlDb struct {
	Db *sql.DB
}

func NewMysqlDb(cs string) (*MysqlDb, error) {
	db, err := sql.Open("mysql", cs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect mysql")
	}
	return &MysqlDb{Db: db}, nil
}

func (p *MysqlDb) Check(ctx context.Context) (string, error) {
	subCtx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	var output int
	if err := p.Db.QueryRowContext(subCtx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = 'public';
`).Scan(&output); err != nil {
		return "", err
	} else {
		return strconv.Itoa(output), nil
	}
}
