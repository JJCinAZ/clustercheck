package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type tcpHealthSettings struct {
	GaleraBindAddress string        `yaml:"galera_bind_address" env:"GALERA_TCP_BIND_ADDRESS"`
	ValkeyBindAddress string        `yaml:"valkey_bind_address" env:"VALKEY_TCP_BIND_ADDRESS"`
	Timeout           time.Duration `yaml:"timeout" env:"TCP_HEALTH_TIMEOUT"`
	galeraAddressSet  bool
	valkeyAddressSet  bool
	timeoutSet        bool
}

func (config *tcpHealthSettings) UnmarshalYAML(value *yaml.Node) error {
	type rawSettings tcpHealthSettings
	var (
		decoded rawSettings
		err     error
	)

	if err = value.Decode(&decoded); err != nil {
		return err
	}
	*config = tcpHealthSettings(decoded)
	config.galeraAddressSet = yamlKeySet(value, "galera_bind_address")
	config.valkeyAddressSet = yamlKeySet(value, "valkey_bind_address")
	config.timeoutSet = yamlKeySet(value, "timeout")
	return nil
}

func validateTCPAddress(name, address string) error {
	var (
		portRaw string
		port    int
		err     error
	)

	if _, portRaw, err = net.SplitHostPort(address); err != nil {
		return fmt.Errorf("%s must be formatted as host:port: %w", name, err)
	}
	if port, err = strconv.Atoi(portRaw); err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s port must be between 1 and 65535", name)
	}
	return nil
}

func validateTCPHealthSettings(config *tcpHealthSettings) error {
	var err error

	if !config.galeraAddressSet && config.GaleraBindAddress == "" {
		config.GaleraBindAddress = ":9200"
	}
	if !config.valkeyAddressSet && config.ValkeyBindAddress == "" {
		config.ValkeyBindAddress = ":9201"
	}
	if !config.timeoutSet && config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	config.GaleraBindAddress = strings.TrimSpace(config.GaleraBindAddress)
	config.ValkeyBindAddress = strings.TrimSpace(config.ValkeyBindAddress)
	// An explicitly blank address disables that listener.
	if config.GaleraBindAddress != "" {
		if err = validateTCPAddress("tcp.galera_bind_address", config.GaleraBindAddress); err != nil {
			return err
		}
	}
	if config.ValkeyBindAddress != "" {
		if err = validateTCPAddress("tcp.valkey_bind_address", config.ValkeyBindAddress); err != nil {
			return err
		}
	}
	if config.Timeout <= 0 {
		return fmt.Errorf("tcp.timeout must be positive")
	}
	return nil
}

func (a *App) serve(handler http.Handler) error {
	var (
		listeners []net.Listener
		listener  net.Listener
		addresses []string
		errCh     chan error
		server    *http.Server
		err       error
	)

	addresses = []string{a.Config.Server.BindAddress, a.Config.TCP.GaleraBindAddress, a.Config.TCP.ValkeyBindAddress}
	listeners = make([]net.Listener, len(addresses))
	defer func() {
		for _, listener := range listeners {
			if listener != nil {
				_ = listener.Close()
			}
		}
	}()
	// Bind everything before serving so a port conflict fails startup as a unit.
	for i, address := range addresses {
		if address == "" {
			continue
		}
		if listener, err = net.Listen("tcp", address); err != nil {
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		listeners[i] = listener
	}

	errCh = make(chan error, len(listeners))
	server = &http.Server{Handler: handler}
	defer server.Close()
	log.Printf("Starting HTTP server on %s", listeners[0].Addr())
	go func() { errCh <- server.Serve(listeners[0]) }()
	if listeners[1] != nil {
		log.Printf("Starting Galera TCP health check on %s", listeners[1].Addr())
		go func() { errCh <- serveTCPHealth(listeners[1], a.Config.TCP.Timeout, a.checkGaleraWritable, "OK\r\n") }()
	}
	if listeners[2] != nil {
		log.Printf("Starting Valkey TCP health check on %s", listeners[2].Addr())
		go func() {
			errCh <- serveTCPHealth(listeners[2], a.Config.TCP.Timeout, a.checkValkeyMaster, "role:master\r\n")
		}()
	}
	return <-errCh
}

func serveTCPHealth(listener net.Listener, timeout time.Duration, check func(context.Context) error, payload string) error {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		slots  chan struct{}
		conn   net.Conn
		err    error
	)

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	slots = make(chan struct{}, 32)
	for {
		if conn, err = listener.Accept(); err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
			go func(conn net.Conn) {
				defer func() { <-slots }()
				handleTCPHealth(ctx, conn, timeout, check, payload)
			}(conn)
		default:
			_ = conn.Close()
		}
	}
}

func handleTCPHealth(parent context.Context, conn net.Conn, timeout time.Duration, check func(context.Context) error, payload string) {
	var (
		ctx      context.Context
		cancel   context.CancelFunc
		deadline time.Time
		err      error
	)

	defer conn.Close()
	ctx, cancel = context.WithTimeout(parent, timeout)
	defer cancel()
	deadline, _ = ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return
	}
	if err = check(ctx); err != nil || ctx.Err() != nil {
		if err != nil {
			_, _ = io.WriteString(conn, err.Error()+"\r\n")
		} else {
			_, _ = io.WriteString(conn, ctx.Err().Error()+"\r\n")
		}
		return
	}
	_, _ = io.WriteString(conn, payload)
}

func queryGaleraValues(ctx context.Context, db *sql.DB, query string) (map[string]string, error) {
	var (
		rows   *sql.Rows
		values map[string]string
		name   string
		value  string
		err    error
	)

	if rows, err = db.QueryContext(ctx, query); err != nil {
		return nil, err
	}
	defer rows.Close()
	values = make(map[string]string)
	for rows.Next() {
		if err = rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		values[strings.ToLower(name)] = value
	}
	return values, rows.Err()
}

func (a *App) checkGaleraWritable(ctx context.Context) error {
	var (
		status    map[string]string
		variables map[string]string
		err       error
	)

	if status, err = queryGaleraValues(ctx, a.DB, `SHOW GLOBAL STATUS WHERE Variable_name IN
		('wsrep_cluster_status', 'wsrep_connected', 'wsrep_local_state', 'wsrep_ready')`); err != nil {
		return err
	}
	if status["wsrep_cluster_status"] != "Primary" || status["wsrep_connected"] != "ON" ||
		status["wsrep_local_state"] != "4" || status["wsrep_ready"] != "ON" {
		return fmt.Errorf("Galera node is not Primary, connected, synced and ready")
	}
	if variables, err = queryGaleraValues(ctx, a.DB, `SHOW GLOBAL VARIABLES WHERE Variable_name IN
		('read_only', 'super_read_only', 'wsrep_on', 'wsrep_reject_queries')`); err != nil {
		return err
	}
	if variables["wsrep_on"] != "ON" {
		return fmt.Errorf("Galera replication is disabled")
	}
	if variables["read_only"] != "OFF" || variables["wsrep_reject_queries"] != "NONE" {
		return fmt.Errorf("Galera node is read-only or rejects queries")
	}
	// MariaDB does not expose super_read_only, but MySQL/Percona may enable it.
	if value, exists := variables["super_read_only"]; exists && value != "OFF" {
		return fmt.Errorf("Galera node has super_read_only enabled")
	}
	return nil
}

func (a *App) checkValkeyMaster(ctx context.Context) error {
	var (
		conn     net.Conn
		client   *respClient
		dialer   net.Dialer
		deadline time.Time
		value    respValue
		err      error
		stop     func() bool
	)

	if conn, err = dialer.DialContext(ctx, "tcp", a.Config.Valkey.Address); err != nil {
		return err
	}
	defer conn.Close()
	// Closing on cancellation bounds the entire AUTH + ROLE exchange.
	stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ = ctx.Deadline()
	client = &respClient{conn: conn, reader: bufio.NewReader(conn), timeout: time.Until(deadline)}
	if a.Config.Valkey.ValkeyPassword != "" {
		if value, err = client.command("AUTH", a.Config.Valkey.ValkeyPassword); err != nil {
			return err
		}
		if !respStringEquals(value, "OK") {
			return fmt.Errorf("Valkey authentication failed")
		}
	}
	if value, err = client.command("ROLE"); err != nil {
		return err
	}
	if value.Kind != '*' || len(value.Array) == 0 || value.Array[0].Kind != '$' || value.Array[0].String != "master" {
		return fmt.Errorf("Valkey node is not a master")
	}
	return nil
}
