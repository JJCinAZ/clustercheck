package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func writeTempConfig(t *testing.T, body string) string {
	var (
		path string
		err  error
	)

	t.Helper()

	path = filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadConfigReadsYAML(t *testing.T) {
	var (
		path   string
		config clustercheckConfig
		err    error
	)

	path = writeTempConfig(t, `
server:
  bind_address: ":9090"
database:
  server: "127.0.0.1:3306"
  user: "cluster_monitor"
  password: "secret"
  db: ""
cluster:
  expected_size: 3
valkey:
  sentinel_addrs:
    - "127.0.0.1:26379"
  master_name: "mymaster"
  expected_nodes: 3
  timeout: 1s
proxmox:
  endpoint: "http://127.0.0.1:8006"
  api_token: "root@pam!clustercheck=secret"
  timeout: 2s
`)

	config, err = loadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.Server.BindAddress != ":9090" {
		t.Fatalf("expected bind address :9090, got %q", config.Server.BindAddress)
	}
	if config.Database.ConnectString != "cluster_monitor:secret@tcp(127.0.0.1:3306)/" {
		t.Fatalf("unexpected connect string: %q", config.Database.ConnectString)
	}
	if config.Cluster.ExpectedSize != 3 {
		t.Fatalf("expected cluster size 3, got %d", config.Cluster.ExpectedSize)
	}
	if config.Valkey.Timeout != time.Second {
		t.Fatalf("expected Valkey timeout 1s, got %s", config.Valkey.Timeout)
	}
	if !config.Proxmox.Enabled {
		t.Fatalf("expected Proxmox config to be enabled")
	}
	if config.Proxmox.Timeout != 2*time.Second {
		t.Fatalf("expected Proxmox timeout 2s, got %s", config.Proxmox.Timeout)
	}
}

func validConfigBody(extra string) string {
	return fmt.Sprintf(`
database:
  server: "127.0.0.1:3306"
  user: "cluster_monitor"
  password: "secret"
  db: ""
cluster:
  expected_size: 3
%s`, extra)
}

func TestLoadConfigAppliesDefaultsForOmittedValues(t *testing.T) {
	var (
		path   string
		config clustercheckConfig
		err    error
	)

	path = writeTempConfig(t, validConfigBody(""))

	config, err = loadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.Server.BindAddress != ":8080" {
		t.Fatalf("expected default bind address :8080, got %q", config.Server.BindAddress)
	}
	if len(config.Valkey.SentinelAddrs) != 1 || config.Valkey.SentinelAddrs[0] != "127.0.0.1:26379" {
		t.Fatalf("unexpected default Valkey sentinel addresses: %#v", config.Valkey.SentinelAddrs)
	}
	if config.Valkey.MasterName != "mymaster" {
		t.Fatalf("expected default Valkey master mymaster, got %q", config.Valkey.MasterName)
	}
	if config.Valkey.ExpectedNodes != 3 {
		t.Fatalf("expected default Valkey node count 3, got %d", config.Valkey.ExpectedNodes)
	}
	if config.Valkey.Timeout != 5*time.Second {
		t.Fatalf("expected default Valkey timeout 5s, got %s", config.Valkey.Timeout)
	}
	if config.Proxmox.Enabled {
		t.Fatalf("expected Proxmox config to be disabled by default")
	}
	if config.Proxmox.Timeout != 10*time.Second {
		t.Fatalf("expected default Proxmox timeout 10s, got %s", config.Proxmox.Timeout)
	}
}

func TestLoadConfigRejectsHostileValues(t *testing.T) {
	var (
		testCases []struct {
			name       string
			body       string
			wantSubstr string
		}
		i  int
		tc struct {
			name       string
			body       string
			wantSubstr string
		}
		path string
		err  error
	)

	testCases = []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{
			name: "blank-bind-address",
			body: validConfigBody(`server:
  bind_address: ""
`),
			wantSubstr: "server.bind_address must not be blank",
		},
		{
			name: "invalid-bind-address-port",
			body: validConfigBody(`server:
  bind_address: ":0"
`),
			wantSubstr: "server.bind_address port must be between 1 and 65535",
		},
		{
			name: "missing-cluster-size",
			body: `
database:
  server: "127.0.0.1:3306"
  user: "cluster_monitor"
  password: "secret"
  db: ""
`,
			wantSubstr: "cluster.expected_size must be a positive integer",
		},
		{
			name: "zero-cluster-size",
			body: `
database:
  server: "127.0.0.1:3306"
  user: "cluster_monitor"
  password: "secret"
  db: ""
cluster:
  expected_size: 0
`,
			wantSubstr: "cluster.expected_size must be a positive integer",
		},
		{
			name: "empty-valkey-sentinels",
			body: validConfigBody(`valkey:
  sentinel_addrs: []
`),
			wantSubstr: "valkey.sentinel_addrs must include at least one host:port",
		},
		{
			name: "blank-valkey-master",
			body: validConfigBody(`valkey:
  master_name: ""
`),
			wantSubstr: "valkey.master_name must not be blank",
		},
		{
			name: "zero-valkey-nodes",
			body: validConfigBody(`valkey:
  expected_nodes: 0
`),
			wantSubstr: "valkey.expected_nodes must be a positive integer",
		},
		{
			name: "zero-valkey-timeout",
			body: validConfigBody(`valkey:
  timeout: 0s
`),
			wantSubstr: "valkey.timeout must be positive",
		},
		{
			name: "bad-valkey-sentinel-port",
			body: validConfigBody(`valkey:
  sentinel_addrs:
    - "127.0.0.1:bad"
`),
			wantSubstr: "valkey.sentinel_addrs ports must be numeric",
		},
		{
			name: "proxmox-endpoint-without-token",
			body: validConfigBody(`proxmox:
  endpoint: "https://p1.acme.com:8006"
`),
			wantSubstr: "proxmox.api_token must be set",
		},
		{
			name: "proxmox-token-without-endpoint",
			body: validConfigBody(`proxmox:
  api_token: "root@pam!clustercheck=secret"
`),
			wantSubstr: "proxmox.endpoint must be set",
		},
		{
			name: "bad-proxmox-scheme",
			body: validConfigBody(`proxmox:
  endpoint: "ftp://p1.acme.com:8006"
  api_token: "root@pam!clustercheck=secret"
`),
			wantSubstr: "proxmox.endpoint must use http or https",
		},
		{
			name: "zero-proxmox-timeout",
			body: validConfigBody(`proxmox:
  endpoint: "https://p1.acme.com:8006"
  api_token: "root@pam!clustercheck=secret"
  timeout: 0s
`),
			wantSubstr: "proxmox.timeout must be positive",
		},
	}

	for i = 0; i < len(testCases); i++ {
		tc = testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			path = writeTempConfig(t, tc.body)
			_, err = loadConfig(path)
			if err == nil {
				t.Fatalf("expected load config error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantSubstr, err.Error())
			}
		})
	}
}

func startFakeProxmoxServer(t *testing.T, cephHealth string, smartHealth map[string]string) *httptest.Server {
	var server *httptest.Server

	t.Helper()

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			disk string
		)

		if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!clustercheck=secret" {
			t.Errorf("unexpected authorization header: %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/cluster/ceph/status":
			fmt.Fprintf(w, `{"data":{"health":{"status":%q}}}`, cephHealth)
		case "/api2/json/cluster/status":
			fmt.Fprint(w, `{"data":[{"type":"node","name":"p1","online":1},{"type":"node","name":"p2","online":1}]}`)
		case "/api2/json/nodes/p1/disks/list":
			fmt.Fprint(w, `{"data":[{"devpath":"/dev/sda","model":"Samsung","serial":"abc","type":"ssd"}]}`)
		case "/api2/json/nodes/p2/disks/list":
			fmt.Fprint(w, `{"data":[{"devpath":"/dev/sdb","model":"Seagate","serial":"def","type":"hdd"}]}`)
		case "/api2/json/nodes/p1/disks/smart", "/api2/json/nodes/p2/disks/smart":
			disk = r.URL.Query().Get("disk")
			fmt.Fprintf(w, `{"data":{"health":%q}}`, smartHealth[disk])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCheckProxmoxStorageHealthy(t *testing.T) {
	var (
		server   *httptest.Server
		config   proxmoxConfig
		response proxmoxStatusResponse
		err      error
	)

	server = startFakeProxmoxServer(t, "HEALTH_OK", map[string]string{
		"/dev/sda": "PASSED",
		"/dev/sdb": "PASSED",
	})
	config = proxmoxConfig{
		Endpoint: server.URL,
		APIToken: "root@pam!clustercheck=secret",
		Timeout:  time.Second,
		Enabled:  true,
	}

	response, err = checkProxmoxStorage(t.Context(), server.Client(), config)
	if err != nil {
		t.Fatalf("check Proxmox storage: %v", err)
	}
	if response.Ceph.State != "ok" {
		t.Fatalf("expected ok Ceph state, got %q", response.Ceph.State)
	}
	if len(response.Drives) != 2 {
		t.Fatalf("expected 2 drives, got %d", len(response.Drives))
	}
	if response.Drives[0].State != "ok" || response.Drives[1].State != "ok" {
		t.Fatalf("expected ok drive states, got %#v", response.Drives)
	}
}

func TestCheckProxmoxStorageReportsUnhealthyCephAndSMART(t *testing.T) {
	var (
		server   *httptest.Server
		config   proxmoxConfig
		response proxmoxStatusResponse
		err      error
	)

	server = startFakeProxmoxServer(t, "HEALTH_WARN", map[string]string{
		"/dev/sda": "PASSED",
		"/dev/sdb": "FAILED",
	})
	config = proxmoxConfig{
		Endpoint: server.URL,
		APIToken: "root@pam!clustercheck=secret",
		Timeout:  time.Second,
		Enabled:  true,
	}

	response, err = checkProxmoxStorage(t.Context(), server.Client(), config)
	if err != nil {
		t.Fatalf("check Proxmox storage: %v", err)
	}
	if response.Ceph.State != "warning" {
		t.Fatalf("expected warning Ceph state, got %q", response.Ceph.State)
	}
	if response.Drives[1].State != "critical" {
		t.Fatalf("expected critical drive state, got %#v", response.Drives[1])
	}
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

	response, err = calculateValkeyHealth(valkeyHealthConfig{
		SentinelAddrs: []string{sentinel.Address},
		MasterName:    "mymaster",
		ExpectedNodes: 3,
		Timeout:       time.Second,
	})
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

	response, err = calculateValkeyHealth(valkeyHealthConfig{
		SentinelAddrs: []string{sentinel.Address},
		MasterName:    "mymaster",
		ExpectedNodes: 3,
		Timeout:       time.Second,
	})
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

	response, err = calculateValkeyHealth(valkeyHealthConfig{
		SentinelAddrs: []string{sentinel.Address},
		MasterName:    "mymaster",
		ExpectedNodes: 3,
		Timeout:       time.Second,
	})
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

	response, err = calculateValkeyHealth(valkeyHealthConfig{
		SentinelAddrs: []string{sentinel.Address},
		MasterName:    "mymaster",
		ExpectedNodes: 3,
		Timeout:       time.Second,
	})
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
