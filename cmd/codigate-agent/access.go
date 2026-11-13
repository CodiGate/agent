package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/codigate/agent/access"
	"github.com/codigate/agent/tunnel"
)

const accessUsage = `usage: codigate-agent access tcp [flags] <hostname>

Connects to a TCP app (agent rule tcp:// or ssh://) through Codigate.

Without --listen it uses stdin and stdout, for SSH:
  ssh -o ProxyCommand="codigate-agent access tcp %h" user@ssh.acme.codigate.app
With --listen it accepts local connections and tunnels each one:
  codigate-agent access tcp --listen 127.0.0.1:5432 db.acme.codigate.app

Protected apps need a service token: --client-id and --client-secret, or
CODIGATE_CLIENT_ID and CODIGATE_CLIENT_SECRET.

flags:
`

func runAccess(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "tcp" {
		fmt.Fprint(stderr, accessUsage)
		return 2
	}
	fs := flag.NewFlagSet("access tcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, accessUsage); fs.PrintDefaults() }
	listen := fs.String("listen", "", "local address to accept connections on (default: stdin/stdout)")
	id := fs.String("client-id", os.Getenv("CODIGATE_CLIENT_ID"), "service token id")
	secret := fs.String("client-secret", os.Getenv("CODIGATE_CLIENT_SECRET"), "service token secret")
	connect := fs.String("connect", "", "edge address host:port (default: <hostname>:443)")
	caFile := fs.String("ca-file", "", "extra CA certificates (PEM) to trust for the edge")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	d := &access.Dialer{Hostname: fs.Arg(0), Address: *connect, ClientID: *id, ClientSecret: *secret}
	if *caFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(*caFile)
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			fmt.Fprintf(stderr, "codigate-agent: --ca-file %q: no usable certificates\n", *caFile)
			return 2
		}
		d.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *listen == "" {
		conn, err := d.Dial(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
			return 1
		}
		defer conn.Close()
		tunnel.Pipe(stdout, stdin, conn)
		return 0
	}

	// Check the app answers before accepting, so mistakes show up at once.
	probe, err := d.Dial(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
		return 1
	}
	probe.Close()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "codigate-agent: %s → %s\n", ln.Addr(), d.Hostname)
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		local, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return 0
			}
			fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
			return 1
		}
		go func() {
			defer local.Close()
			remote, err := d.Dial(ctx)
			if err != nil {
				fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
				return
			}
			defer remote.Close()
			tunnel.Pipe(local, local, remote)
		}()
	}
}
