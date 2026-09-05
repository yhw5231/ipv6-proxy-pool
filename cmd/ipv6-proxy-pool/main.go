package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ipv6-proxy-pool/internal/admin"
	"ipv6-proxy-pool/internal/client"
	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
	"ipv6-proxy-pool/internal/listener"
	"ipv6-proxy-pool/internal/socks5"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "client" {
		runClient(os.Args[2:])
		return
	}

	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	writeDefault := flag.Bool("write-default-config", false, "write a default configuration and exit")
	flag.Parse()

	if *writeDefault {
		if err := config.SaveAtomic(*configPath, config.Default()); err != nil {
			log.Fatalf("write default configuration: %v", err)
		}
		log.Printf("wrote default configuration to %s", *configPath)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, *configPath); err != nil {
		log.Fatalf("run proxy pool: %v", err)
	}
}

func run(ctx context.Context, cfg config.Config, configPath string) error {
	var pool *lease.Pool
	var listenerManager *listener.Manager

	options := lease.Options{
		Prefix:         cfg.IPv6Prefix,
		MinLeases:      cfg.MinLeases,
		MaxLeases:      cfg.MaxLeases,
		IdleTimeout:    cfg.IdleTimeout,
		RotateAfter:    cfg.RotateAfter,
		RotateRequests: cfg.RotateRequests,
	}
	if cfg.SOCKS.Mode == config.ModePerIPv6 {
		options.PortStart = cfg.SOCKS.PortStart
		options.PortEnd = cfg.SOCKS.PortEnd
		// When a lease is released (explicitly or by the idle sweeper) its
		// per-IPv6 listener must be stopped so the port returns to the pool.
		// The closure runs outside the pool mutex and never blocks on it.
		options.OnRelease = func(id string) {
			if listenerManager == nil {
				return
			}
			if _, exists := pool.Get(id); exists {
				return
			}
			if err := listenerManager.Stop(id); err != nil && !errors.Is(err, listener.ErrNotFound) {
				log.Printf("stop listener for released lease %q: %v", id, err)
			}
		}
	}

	var err error
	pool, err = lease.NewPool(options)
	if err != nil {
		return fmt.Errorf("create lease pool: %w", err)
	}

	proxy := socks5.NewServer(pool)
	listenerManager = listener.NewManager(ctx, proxy)
	defer listenerManager.Close()

	log.Printf("resident standby leases ready: %d / %d (min_leases %d, max_leases %d)",
		pool.StandbyCount(), cfg.MinLeases, cfg.MinLeases, cfg.MaxLeases)

	go idleSweeper(ctx, pool, cfg.IdleTimeout)

	adminServer := &http.Server{
		Addr: cfg.Admin.ListenAddress,
		Handler: admin.HandlerWithOptions(pool, admin.Options{
			ConfigPath:      configPath,
			RuntimeConfig:   cfg,
			AdminToken:      cfg.Admin.Token,
			Web:             os.DirFS("web"),
			ListenerManager: listenerManager,
			Probe:           socks5.ProbeProxy,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, cfg.MaxLeases+2)
	go func() {
		log.Printf("management API listening on %s", cfg.Admin.ListenAddress)
		if serveErr := adminServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve management API: %w", serveErr)
		}
	}()

	listeners, err := startSOCKSListeners(ctx, cfg, pool, proxy, listenerManager, errCh)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adminServer.Shutdown(shutdownCtx)
		return err
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}

	for _, listener := range listeners {
		_ = listener.Close()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adminServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shut down management API: %w", err)
	}
	return runErr
}

func startSOCKSListeners(ctx context.Context, cfg config.Config, pool *lease.Pool, proxy *socks5.Server, listenerManager *listener.Manager, errCh chan<- error) ([]net.Listener, error) {
	if cfg.SOCKS.Mode == config.ModeMultiplex {
		ln, err := net.Listen("tcp", cfg.SOCKS.ListenAddress)
		if err != nil {
			return nil, fmt.Errorf("listen for multiplexed SOCKS5: %w", err)
		}
		log.Printf("multiplexed SOCKS5 listening on %s", ln.Addr())
		go serveSOCKS(ctx, proxy, ln, "", errCh)
		return []net.Listener{ln}, nil
	}

	host, _, err := net.SplitHostPort(cfg.SOCKS.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("parse SOCKS listen address: %w", err)
	}
	if listenerManager == nil {
		return nil, errors.New("listener manager is required in per_ipv6 mode")
	}

	startedIDs := make([]string, 0, len(cfg.SOCKS.AlwaysOnPorts))
	createdIDs := make([]string, 0, len(cfg.SOCKS.AlwaysOnPorts))
	rollback := func() {
		for index := len(startedIDs) - 1; index >= 0; index-- {
			_ = listenerManager.Stop(startedIDs[index])
		}
		for index := len(createdIDs) - 1; index >= 0; index-- {
			pool.Release(createdIDs[index])
		}
	}

	for _, port := range cfg.SOCKS.AlwaysOnPorts {
		id := fmt.Sprintf("port-%d", port)
		_, existed := pool.Get(id)
		entry, acquireErr := pool.AcquirePort(id, port, true)
		if acquireErr != nil {
			rollback()
			return nil, fmt.Errorf("create persistent lease %q on port %d: %w", id, port, acquireErr)
		}
		if !existed {
			createdIDs = append(createdIDs, id)
		}

		address := net.JoinHostPort(host, strconv.Itoa(port))
		info, startErr := listenerManager.Start(id, address)
		if startErr != nil {
			rollback()
			return nil, fmt.Errorf("start listener for lease %q on %s: %w", id, address, startErr)
		}
		startedIDs = append(startedIDs, id)
		log.Printf("SOCKS5 lease %s using %s listening on %s", id, entry.IPv6, info.Address)
	}
	return nil, nil
}
func serveSOCKS(ctx context.Context, proxy *socks5.Server, listener net.Listener, leaseID string, errCh chan<- error) {
	if err := proxy.Serve(ctx, listener, leaseID); err != nil {
		select {
		case errCh <- err:
		case <-ctx.Done():
		}
	}
}

func closeListeners(listeners []net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

// idleSweeper periodically releases leases whose idle timeout expired and stops
// their per-IPv6 listeners, so ports and addresses are reclaimed automatically
// without waiting for the next client connection.
func idleSweeper(ctx context.Context, pool *lease.Pool, idleTimeout time.Duration) {
	interval := time.Minute
	if idleTimeout > 0 {
		interval = idleTimeout / 3
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}
		if interval > 2*time.Minute {
			interval = 2 * time.Minute
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if released := pool.ReleaseIdle(); released > 0 {
				log.Printf("idle sweeper released %d lease(s)", released)
			}
		}
	}
}

// runClient implements the "client" subcommand, which requests, rotates and
// releases IPv6 proxy leases through the management API so remote clients do
// not need curl or direct shell access on the server.
//
// The first argument is the verb, everything after it is treated as flags:
//
//	ipv6-proxy-pool client create -name client-a -admin http://host:10070 -token T
func runClient(args []string) {
	if len(args) == 0 || args[0] == "help" {
		client.PrintUsage()
		return
	}
	verb := args[0]
	if strings.HasPrefix(verb, "-") {
		log.Fatalf("client: 第一个参数应为命令（create/rotate/recycle/delete/list/status/help），例如: client create -name client-a")
	}

	flags := flag.NewFlagSet("client", flag.ExitOnError)
	adminURL := flags.String("admin", "http://127.0.0.1:10070", "management API base URL")
	token := flags.String("token", os.Getenv("IPV6_PROXY_POOL_TOKEN"), "admin token (or IPV6_PROXY_POOL_TOKEN env)")
	server := flags.String("server", "", "public host/IP used to build the SOCKS5 endpoint (defaults to admin host)")
	name := flags.String("name", "", "client/lease identifier")
	persistent := flags.Bool("persistent", false, "exempt the lease from idle release")
	if err := flags.Parse(args[1:]); err != nil {
		log.Fatalf("client: %v", err)
	}

	opts := client.Options{AdminURL: *adminURL, Token: *token, Server: *server}

	var err error
	switch verb {
	case "create", "new":
		if *name == "" {
			log.Fatal("client create requires -name")
		}
		err = client.Create(opts, *name, *persistent)
	case "rotate", "renew":
		if *name == "" {
			log.Fatal("client rotate requires -name")
		}
		err = client.Rotate(opts, *name)
	case "recycle":
		if *name == "" {
			log.Fatal("client recycle requires -name")
		}
		err = client.Recycle(opts, *name)
	case "delete", "remove":
		if *name == "" {
			log.Fatal("client delete requires -name")
		}
		err = client.Delete(opts, *name)
	case "list", "leases":
		err = client.List(opts)
	case "status":
		err = client.Status(opts)
	default:
		log.Fatalf("client: unknown command %q (use: create, rotate, recycle, delete, list, status)", verb)
	}
	if err != nil {
		log.Fatalf("client %s: %v", verb, err)
	}
}
