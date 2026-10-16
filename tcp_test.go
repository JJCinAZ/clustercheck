package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A small database/sql driver exercises the real queries and cancellation
// without requiring a running Galera cluster or another test dependency.
type galeraTestConnector struct {
	query func(context.Context, string) (driver.Rows, error)
}

type galeraTestDriver struct{}

type galeraTestConn struct {
	query func(context.Context, string) (driver.Rows, error)
}

type galeraTestRows struct {
	values [][]driver.Value
}

func (c galeraTestConnector) Connect(context.Context) (driver.Conn, error) {
	return galeraTestConn{query: c.query}, nil
}

func (c galeraTestConnector) Driver() driver.Driver { return galeraTestDriver{} }

func (galeraTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

func (galeraTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}

func (galeraTestConn) Close() error { return nil }

func (galeraTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}

func (c galeraTestConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	return c.query(ctx, query)
}

func (r *galeraTestRows) Columns() []string { return []string{"Variable_name", "Value"} }

func (r *galeraTestRows) Close() error { return nil }

func (r *galeraTestRows) Next(values []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(values, r.values[0])
	r.values = r.values[1:]
	return nil
}

func galeraTestDB(t *testing.T, query func(context.Context, string) (driver.Rows, error)) *sql.DB {
	var db *sql.DB

	t.Helper()
	db = sql.OpenDB(galeraTestConnector{query: query})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestGaleraWritable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overrides  map[string]string
		queryError bool
		wantOK     bool
	}{
		{name: "writable", wantOK: true},
		{name: "super-read-only-supported-off", overrides: map[string]string{"super_read_only": "OFF"}, wantOK: true},
		{name: "non-primary", overrides: map[string]string{"wsrep_cluster_status": "Non-Primary"}},
		{name: "disconnected", overrides: map[string]string{"wsrep_connected": "OFF"}},
		{name: "not-ready", overrides: map[string]string{"wsrep_ready": "OFF"}},
		{name: "joining", overrides: map[string]string{"wsrep_local_state": "1"}},
		{name: "donor", overrides: map[string]string{"wsrep_local_state": "2"}},
		{name: "joined-unsynced", overrides: map[string]string{"wsrep_local_state": "3"}},
		{name: "read-only", overrides: map[string]string{"read_only": "ON"}},
		{name: "super-read-only", overrides: map[string]string{"super_read_only": "ON"}},
		{name: "replication-disabled", overrides: map[string]string{"wsrep_on": "OFF"}},
		{name: "reject-queries", overrides: map[string]string{"wsrep_reject_queries": "ALL"}},
		{name: "missing-status", overrides: map[string]string{"wsrep_ready": ""}},
		{name: "missing-read-only", overrides: map[string]string{"read_only": ""}},
		{name: "query-error", queryError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				app    App
				values map[string]string
				err    error
			)

			values = map[string]string{
				"wsrep_cluster_status": "Primary", "wsrep_connected": "ON",
				"wsrep_local_state": "4", "wsrep_ready": "ON",
				"read_only": "OFF", "wsrep_reject_queries": "NONE", "wsrep_on": "ON",
			}
			for key, value := range tc.overrides {
				values[key] = value
			}
			app.DB = galeraTestDB(t, func(ctx context.Context, query string) (driver.Rows, error) {
				var rows galeraTestRows

				if tc.queryError {
					return nil, errors.New("database unavailable")
				}
				if !strings.HasPrefix(query, "SHOW GLOBAL STATUS WHERE") && !strings.HasPrefix(query, "SHOW GLOBAL VARIABLES WHERE") {
					return nil, fmt.Errorf("unexpected query: %s", query)
				}
				// Only return variables explicitly requested by the actual query.
				for name, value := range values {
					if strings.Contains(query, "'"+name+"'") && value != "" {
						rows.values = append(rows.values, []driver.Value{name, value})
					}
				}
				return &rows, nil
			})
			err = app.checkGaleraWritable(t.Context())
			if (err == nil) != tc.wantOK {
				t.Fatalf("writable = %v, want %v; error: %v", err == nil, tc.wantOK, err)
			}
		})
	}
}

func startTCPProbe(t *testing.T, timeout time.Duration, check func(context.Context) error, payload string) string {
	var (
		listener net.Listener
		done     chan error
		err      error
	)

	t.Helper()
	if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	done = make(chan error, 1)
	go func() { done <- serveTCPHealth(listener, timeout, check, payload) }()
	t.Cleanup(func() {
		_ = listener.Close()
		if err = <-done; !errors.Is(err, net.ErrClosed) {
			t.Errorf("unexpected listener error: %v", err)
		}
	})
	return listener.Addr().String()
}

func readTCPProbe(t *testing.T, address string) string {
	var (
		conn    net.Conn
		payload []byte
		err     error
	)

	t.Helper()
	if conn, err = net.DialTimeout("tcp", address, time.Second); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if payload, err = io.ReadAll(conn); err != nil {
		t.Fatalf("read probe: %v", err)
	}
	return string(payload)
}

func TestTCPHealthResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		fail    bool
	}{
		{name: "galera", payload: "OK\r\n"},
		{name: "valkey", payload: "role:master\r\n"},
		{name: "unhealthy", payload: "OK\r\n", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				address string
				want    string
			)

			address = startTCPProbe(t, time.Second, func(context.Context) error {
				if tc.fail {
					return errors.New("unhealthy")
				}
				return nil
			}, tc.payload)
			if !tc.fail {
				want = tc.payload
			}
			if got := readTCPProbe(t, address); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestTCPGaleraQueryTimeout(t *testing.T) {
	var (
		app      App
		address  string
		canceled chan struct{}
	)

	canceled = make(chan struct{})
	app.DB = galeraTestDB(t, func(ctx context.Context, query string) (driver.Rows, error) {
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	})
	address = startTCPProbe(t, 30*time.Millisecond, app.checkGaleraWritable, "OK\r\n")
	if got := readTCPProbe(t, address); got != "" {
		t.Fatalf("timed out query returned %q", got)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("database query was not canceled")
	}
}

func TestTCPValkeyMasterAndDemotion(t *testing.T) {
	var (
		app     App
		server  fakeRESPServer
		address string
		demoted atomic.Bool
		auth    atomic.Int32
		role    atomic.Int32
	)

	server = startFakeRESPServer(t, func(args []string) string {
		if len(args) == 2 && args[0] == "AUTH" && args[1] == "secret" {
			auth.Add(1)
			return respSimple("OK")
		}
		if len(args) == 1 && args[0] == "ROLE" {
			role.Add(1)
			if demoted.Load() {
				return respArrayStrings("slave", "127.0.0.1", "6379", "connected", "0")
			}
			return respArrayValues(respBulk("master"), ":0\r\n", "*0\r\n")
		}
		return respError("ERR unexpected command")
	})
	app.Config.Valkey = valkeyHealthConfig{Address: server.Address, ValkeyPassword: "secret"}
	address = startTCPProbe(t, time.Second, app.checkValkeyMaster, "role:master\r\n")
	if got := readTCPProbe(t, address); got != "role:master\r\n" {
		t.Fatalf("master returned %q", got)
	}
	demoted.Store(true)
	if got := readTCPProbe(t, address); got != "" {
		t.Fatalf("demoted node returned %q", got)
	}
	if auth.Load() != 2 || role.Load() != 2 {
		t.Fatalf("expected fresh AUTH and ROLE per probe; got auth=%d role=%d", auth.Load(), role.Load())
	}
}

func TestTCPValkeyFailures(t *testing.T) {
	for _, tc := range []struct {
		reply    string
		password string
	}{
		{reply: respError("NOAUTH Authentication required")},
		{reply: respArrayStrings("slave")},
		{reply: respArrayStrings("sentinel")},
		{reply: "*0\r\n"},
		{reply: respSimple("master")},
		{reply: respError("WRONGPASS invalid password"), password: "wrong"},
		{reply: respSimple("unexpected AUTH reply"), password: "secret"},
	} {
		t.Run(fmt.Sprintf("reply-%q", tc.reply), func(t *testing.T) {
			var (
				app     App
				server  fakeRESPServer
				address string
			)

			server = startFakeRESPServer(t, func([]string) string { return tc.reply })
			app.Config.Valkey = valkeyHealthConfig{Address: server.Address, ValkeyPassword: tc.password}
			address = startTCPProbe(t, time.Second, app.checkValkeyMaster, "role:master\r\n")
			if got := readTCPProbe(t, address); got != "" {
				t.Fatalf("unhealthy Valkey node returned %q", got)
			}
		})
	}
}

func TestTCPValkeyTimeout(t *testing.T) {
	var (
		app      App
		listener net.Listener
		done     chan struct{}
		address  string
		err      error
	)

	if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done = make(chan struct{})
	go func() {
		var (
			conn net.Conn
			err  error
		)

		defer close(done)
		if conn, err = listener.Accept(); err != nil {
			return
		}
		defer conn.Close()
		// Consume commands without replying. The probe must cancel AUTH.
		_, _ = io.Copy(io.Discard, conn)
	}()
	app.Config.Valkey = valkeyHealthConfig{Address: listener.Addr().String(), ValkeyPassword: "secret"}
	address = startTCPProbe(t, 30*time.Millisecond, app.checkValkeyMaster, "role:master\r\n")
	if got := readTCPProbe(t, address); got != "" {
		t.Fatalf("timed out backend returned %q", got)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backend connection was not closed on timeout")
	}
}

func TestTCPHealthConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		galera string
		valkey string
		node   string
	}{
		{name: "defaults", galera: ":9200", valkey: ":9201", node: "127.0.0.1:6379"},
		{name: "custom", body: "tcp:\n  galera_bind_address: '127.0.0.1:9300'\n  valkey_bind_address: '[::1]:9301'\nvalkey:\n  address: '127.0.0.2:6380'\n", galera: "127.0.0.1:9300", valkey: "[::1]:9301", node: "127.0.0.2:6380"},
		{name: "disabled", body: "tcp:\n  galera_bind_address: ''\n  valkey_bind_address: ''\n", node: "127.0.0.1:6379"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				config clustercheckConfig
				err    error
			)

			if config, err = loadConfig(writeTempConfig(t, validConfigBody(tc.body))); err != nil {
				t.Fatal(err)
			}
			if config.TCP.GaleraBindAddress != tc.galera || config.TCP.ValkeyBindAddress != tc.valkey || config.Valkey.Address != tc.node || config.TCP.Timeout != 2*time.Second {
				t.Fatalf("unexpected config: tcp=%+v valkey.address=%s", config.TCP, config.Valkey.Address)
			}
		})
	}
	for _, body := range []string{
		"tcp:\n  galera_bind_address: ':0'\n",
		"tcp:\n  valkey_bind_address: 'bad'\n",
		"tcp:\n  timeout: 0s\n",
		"tcp:\n  timeout: -1s\n",
		"valkey:\n  address: ''\n",
		"valkey:\n  address: '127.0.0.1:65536'\n",
	} {
		if _, err := loadConfig(writeTempConfig(t, validConfigBody(body))); err == nil {
			t.Errorf("accepted invalid config: %s", body)
		}
	}
}

func TestServeFailsOnOccupiedTCPPort(t *testing.T) {
	var (
		app      App
		listener net.Listener
		err      error
	)

	if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app.Config.Server.BindAddress = "127.0.0.1:0"
	app.Config.TCP.GaleraBindAddress = listener.Addr().String()
	if err = app.serve(http.NewServeMux()); err == nil {
		t.Fatal("expected startup to fail on occupied TCP port")
	}
}
