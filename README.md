# demo-app

A small http UI echo server to use as a platform engineering example application. This listens with HTTP on port `8080`
and outputs various facts about the request and server.

![screenshot of demo-app](./screenshot.png)

## Docker image for Linux

```sh
docker pull ghcr.io/astromechza/demo-app:v0.1.0
```

## Go binary

To install the binary into your own image or system, do the following and it should be available on `$GOPATH/bin/demo-app`.

```
go install github.com/astromechza/demo-app@v0.1.0
```

## Options

The following flags are available and may also be set through the `OVERRIDE_<flag uppercase>` environment variables.

```
  -color string
    	the background color to display (default "random")
  -listen string
    	the address to listen on (default ":8080")
  -motd string
    	specify a message of the day, prefix with '@' to read from a file (default "Hello World")
  -proxy string
    	forward the request to the given http or https endpoint
```
