package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

type fakeRESPServer struct {
	Address  string
	listener net.Listener
}

func startFakeRESPServer(t *testing.T, handler func([]string) string) fakeRESPServer {
	var (
		server   fakeRESPServer
		listener net.Listener
		err      error
	)

	t.Helper()

	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake RESP server: %v", err)
	}

	server = fakeRESPServer{
		Address:  listener.Addr().String(),
		listener: listener,
	}
	t.Cleanup(func() {
		_ = server.listener.Close()
	})

	go func() {
		var (
			conn net.Conn
			err  error
		)

		for {
			conn, err = listener.Accept()
			if err != nil {
				return
			}
			go handleFakeRESPConnection(t, conn, handler)
		}
	}()

	return server
}

func handleFakeRESPConnection(t *testing.T, conn net.Conn, handler func([]string) string) {
	var (
		reader *bufio.Reader
		value  respValue
		err    error
		args   []string
		reply  string
	)

	defer func() {
		_ = conn.Close()
	}()

	reader = bufio.NewReader(conn)
	for {
		value, err = readRESPValue(reader)
		if err != nil {
			return
		}
		args = respRequestArgs(value)
		reply = handler(args)
		if reply == "" {
			reply = "-ERR unsupported command\r\n"
		}
		if _, err = conn.Write([]byte(reply)); err != nil {
			t.Errorf("write fake RESP response: %v", err)
			return
		}
	}
}

func respRequestArgs(value respValue) []string {
	var (
		args []string
		i    int
	)

	args = make([]string, 0)
	if value.Kind != '*' {
		return args
	}
	for i = 0; i < len(value.Array); i++ {
		args = append(args, respValueString(value.Array[i]))
	}
	return args
}

func respSimple(value string) string {
	return fmt.Sprintf("+%s\r\n", value)
}

func respError(value string) string {
	return fmt.Sprintf("-%s\r\n", value)
}

func respBulk(value string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(value), value)
}

func respArrayStrings(values ...string) string {
	var (
		builder strings.Builder
		i       int
	)

	builder.WriteString(fmt.Sprintf("*%d\r\n", len(values)))
	for i = 0; i < len(values); i++ {
		builder.WriteString(respBulk(values[i]))
	}
	return builder.String()
}

func respArrayValues(values ...string) string {
	var (
		builder strings.Builder
		i       int
	)

	builder.WriteString(fmt.Sprintf("*%d\r\n", len(values)))
	for i = 0; i < len(values); i++ {
		builder.WriteString(values[i])
	}
	return builder.String()
}

func splitHostPort(t *testing.T, address string) (string, string) {
	var (
		host string
		port string
		err  error
	)

	t.Helper()

	host, port, err = net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split host port %q: %v", address, err)
	}
	return host, port
}

func startPingServer(t *testing.T) fakeRESPServer {
	return startFakeRESPServer(t, func(args []string) string {
		if len(args) == 1 && strings.EqualFold(args[0], "PING") {
			return respSimple("PONG")
		}
		return respError("ERR unsupported command")
	})
}

func startSentinelServer(t *testing.T, master fakeRESPServer, replicas []fakeRESPServer, replicaFlags []string, ckquorumHealthy bool) fakeRESPServer {
	var (
		masterHost string
		masterPort string
	)

	t.Helper()

	masterHost, masterPort = splitHostPort(t, master.Address)

	return startFakeRESPServer(t, func(args []string) string {
		var (
			command     string
			subcommand  string
			replicaHost string
			replicaPort string
			replicaRows []string
			i           int
			flags       string
		)

		if len(args) == 1 && strings.EqualFold(args[0], "PING") {
			return respSimple("PONG")
		}
		if len(args) < 3 || !strings.EqualFold(args[0], "SENTINEL") {
			return respError("ERR unsupported command")
		}

		command = strings.ToUpper(args[0])
		subcommand = strings.ToUpper(args[1])
		if command != "SENTINEL" || args[2] != "mymaster" {
			return respError("ERR unsupported command")
		}

		switch subcommand {
		case "CKQUORUM":
			if !ckquorumHealthy {
				return respError("NOGOODSLAVE Not enough good replicas to write.")
			}
			return respSimple("OK 3 usable Sentinels. Quorum and failover authorization can be reached")
		case "MASTER":
			return respArrayStrings(
				"name", "mymaster",
				"ip", masterHost,
				"port", masterPort,
				"flags", "master",
			)
		case "REPLICAS":
			replicaRows = make([]string, 0, len(replicas))
			for i = 0; i < len(replicas); i++ {
				replicaHost, replicaPort = splitHostPort(t, replicas[i].Address)
				flags = "slave"
				if i < len(replicaFlags) && replicaFlags[i] != "" {
					flags = replicaFlags[i]
				}
				replicaRows = append(replicaRows, respArrayStrings(
					"name", fmt.Sprintf("%s:%s", replicaHost, replicaPort),
					"ip", replicaHost,
					"port", replicaPort,
					"flags", flags,
				))
			}
			return respArrayValues(replicaRows...)
		}

		return respError("ERR unsupported command")
	})
}

func TestCalculateValkeyHealthHealthy(t *testing.T) {
	var (
		master   fakeRESPServer
		replica1 fakeRESPServer
		replica2 fakeRESPServer
		sentinel fakeRESPServer
		response valkeyHealthResponse
		err      error
	)

	master = startPingServer(t)
	replica1 = startPingServer(t)
	replica2 = startPingServer(t)
	sentinel = startSentinelServer(t, master, []fakeRESPServer{replica1, replica2}, nil, true)

	t.Setenv("VALKEY_SENTINEL_ADDRS", sentinel.Address)
	t.Setenv("EXPECTED_VALKEY_NODES", "3")
	t.Setenv("VALKEY_TIMEOUT_SECONDS", "1")

	response, err = calculateValkeyHealth()
	if err != nil {
		t.Fatalf("calculate Valkey health: %v", err)
	}
	if response.State != "ok" {
		t.Fatalf("expected ok state, got %q: %s", response.State, response.Status)
	}
	if len(response.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(response.Nodes))
	}
}

func TestCalculateValkeyHealthMissingReplica(t *testing.T) {
	var (
		master   fakeRESPServer
		replica1 fakeRESPServer
		sentinel fakeRESPServer
		response valkeyHealthResponse
		err      error
	)

	master = startPingServer(t)
	replica1 = startPingServer(t)
	sentinel = startSentinelServer(t, master, []fakeRESPServer{replica1}, nil, true)

	t.Setenv("VALKEY_SENTINEL_ADDRS", sentinel.Address)
	t.Setenv("EXPECTED_VALKEY_NODES", "3")
	t.Setenv("VALKEY_TIMEOUT_SECONDS", "1")

	response, err = calculateValkeyHealth()
	if err != nil {
		t.Fatalf("calculate Valkey health: %v", err)
	}
	if response.State != "critical" {
		t.Fatalf("expected critical state, got %q", response.State)
	}
	if !strings.Contains(response.Status, "expected 3") {
		t.Fatalf("expected node count status, got %q", response.Status)
	}
}

func TestCalculateValkeyHealthUnhealthyReplicaFlag(t *testing.T) {
	var (
		master   fakeRESPServer
		replica1 fakeRESPServer
		replica2 fakeRESPServer
		sentinel fakeRESPServer
		response valkeyHealthResponse
		err      error
	)

	master = startPingServer(t)
	replica1 = startPingServer(t)
	replica2 = startPingServer(t)
	sentinel = startSentinelServer(t, master, []fakeRESPServer{replica1, replica2}, []string{"slave,s_down", "slave"}, true)

	t.Setenv("VALKEY_SENTINEL_ADDRS", sentinel.Address)
	t.Setenv("EXPECTED_VALKEY_NODES", "3")
	t.Setenv("VALKEY_TIMEOUT_SECONDS", "1")

	response, err = calculateValkeyHealth()
	if err != nil {
		t.Fatalf("calculate Valkey health: %v", err)
	}
	if response.State != "critical" {
		t.Fatalf("expected critical state, got %q", response.State)
	}
	if !strings.Contains(response.Status, "s_down") {
		t.Fatalf("expected unhealthy flag status, got %q", response.Status)
	}
}

func TestCalculateValkeyHealthFailedQuorum(t *testing.T) {
	var (
		master   fakeRESPServer
		replica1 fakeRESPServer
		replica2 fakeRESPServer
		sentinel fakeRESPServer
		response valkeyHealthResponse
		err      error
	)

	master = startPingServer(t)
	replica1 = startPingServer(t)
	replica2 = startPingServer(t)
	sentinel = startSentinelServer(t, master, []fakeRESPServer{replica1, replica2}, nil, false)

	t.Setenv("VALKEY_SENTINEL_ADDRS", sentinel.Address)
	t.Setenv("EXPECTED_VALKEY_NODES", "3")
	t.Setenv("VALKEY_TIMEOUT_SECONDS", "1")

	response, err = calculateValkeyHealth()
	if err != nil {
		t.Fatalf("calculate Valkey health: %v", err)
	}
	if response.State != "critical" {
		t.Fatalf("expected critical state, got %q", response.State)
	}
	if !strings.Contains(response.Status, "CKQUORUM") {
		t.Fatalf("expected CKQUORUM status, got %q", response.Status)
	}
}
