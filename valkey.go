package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type valkeyHealthConfig struct {
	Address          string        `yaml:"address" env:"VALKEY_ADDRESS"`
	SentinelAddrs    []string      `yaml:"sentinel_addrs" env:"VALKEY_SENTINEL_ADDRS"`
	MasterName       string        `yaml:"master_name" env:"VALKEY_MASTER_NAME"`
	ExpectedNodes    int           `yaml:"expected_nodes" env:"EXPECTED_VALKEY_NODES"`
	Timeout          time.Duration `yaml:"timeout" env:"VALKEY_TIMEOUT"`
	SentinelPassword string        `yaml:"sentinel_password" env:"VALKEY_SENTINEL_PASSWORD"`
	ValkeyPassword   string        `yaml:"password" env:"VALKEY_PASSWORD"`
	sentinelAddrsSet bool
	masterNameSet    bool
	expectedNodesSet bool
	timeoutSet       bool
	addressSet       bool
}

type valkeyNodeStatus struct {
	Role    string `json:"role"`
	Address string `json:"address"`
	Flags   string `json:"flags,omitempty"`
	State   string `json:"state"`
	Status  string `json:"status"`
}

type valkeySentinelStatus struct {
	Address string `json:"address"`
	State   string `json:"state"`
	Status  string `json:"status"`
}

type valkeyHealthResponse struct {
	State         string                 `json:"state"`
	Status        string                 `json:"status"`
	MasterName    string                 `json:"master_name"`
	ExpectedNodes int                    `json:"expected_nodes"`
	Nodes         []valkeyNodeStatus     `json:"nodes"`
	Sentinels     []valkeySentinelStatus `json:"sentinels"`
	Checks        map[string]string      `json:"checks"`
}

type respValue struct {
	Kind    byte
	String  string
	Integer int64
	Array   []respValue
}

type respClient struct {
	conn    net.Conn
	reader  *bufio.Reader
	timeout time.Duration
}

func validateValkeyHealthConfig(config *valkeyHealthConfig) error {
	var (
		i       int
		addr    string
		portRaw string
		port    int
		err     error
	)

	if !config.addressSet && config.Address == "" {
		config.Address = "127.0.0.1:6379"
	}
	config.Address = strings.TrimSpace(config.Address)
	if err = validateTCPAddress("valkey.address", config.Address); err != nil {
		return err
	}

	if !config.sentinelAddrsSet {
		if len(config.SentinelAddrs) == 0 {
			config.SentinelAddrs = []string{"127.0.0.1:26379"}
		}
	}
	if !config.masterNameSet {
		if config.MasterName == "" {
			config.MasterName = "mymaster"
		}
	}
	if !config.expectedNodesSet {
		if config.ExpectedNodes == 0 {
			config.ExpectedNodes = 3
		}
	}
	if !config.timeoutSet {
		if config.Timeout == 0 {
			config.Timeout = 5 * time.Second
		}
	}
	if len(config.SentinelAddrs) == 0 {
		return fmt.Errorf("valkey.sentinel_addrs must include at least one host:port")
	}
	for i = 0; i < len(config.SentinelAddrs); i++ {
		addr = strings.TrimSpace(config.SentinelAddrs[i])
		config.SentinelAddrs[i] = addr
		if addr == "" {
			return fmt.Errorf("valkey.sentinel_addrs must not include blank addresses")
		}
		_, portRaw, err = net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("valkey.sentinel_addrs must contain host:port values: %w", err)
		}
		if portRaw == "" {
			return fmt.Errorf("valkey.sentinel_addrs must include ports")
		}
		port, err = strconv.Atoi(portRaw)
		if err != nil {
			return fmt.Errorf("valkey.sentinel_addrs ports must be numeric")
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("valkey.sentinel_addrs ports must be between 1 and 65535")
		}
	}
	if strings.TrimSpace(config.MasterName) == "" {
		return fmt.Errorf("valkey.master_name must not be blank")
	}
	config.MasterName = strings.TrimSpace(config.MasterName)
	if config.ExpectedNodes < 1 {
		return fmt.Errorf("valkey.expected_nodes must be a positive integer")
	}
	if config.Timeout <= 0 {
		return fmt.Errorf("valkey.timeout must be positive")
	}
	return nil
}

func encodeRESPCommand(args []string) []byte {
	var (
		builder strings.Builder
		i       int
	)

	builder.WriteString("*")
	builder.WriteString(strconv.Itoa(len(args)))
	builder.WriteString("\r\n")
	for i = 0; i < len(args); i++ {
		builder.WriteString("$")
		builder.WriteString(strconv.Itoa(len(args[i])))
		builder.WriteString("\r\n")
		builder.WriteString(args[i])
		builder.WriteString("\r\n")
	}
	return []byte(builder.String())
}

func trimRESPLine(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

func readRESPValue(reader *bufio.Reader) (respValue, error) {
	var (
		prefix  byte
		line    string
		err     error
		size    int
		count   int
		payload []byte
		values  []respValue
		value   respValue
		i       int
	)

	prefix, err = reader.ReadByte()
	if err != nil {
		return respValue{}, err
	}

	line, err = reader.ReadString('\n')
	if err != nil {
		return respValue{}, err
	}
	line = trimRESPLine(line)

	switch prefix {
	case '+':
		return respValue{Kind: prefix, String: line}, nil
	case '-':
		return respValue{}, fmt.Errorf("valkey returned error: %s", line)
	case ':':
		value.Integer, err = strconv.ParseInt(line, 10, 64)
		if err != nil {
			return respValue{}, fmt.Errorf("parse RESP integer: %w", err)
		}
		value.Kind = prefix
		return value, nil
	case '$':
		size, err = strconv.Atoi(line)
		if err != nil {
			return respValue{}, fmt.Errorf("parse RESP bulk length: %w", err)
		}
		if size < 0 {
			return respValue{Kind: prefix}, nil
		}
		payload = make([]byte, size+2)
		if _, err = io.ReadFull(reader, payload); err != nil {
			return respValue{}, err
		}
		return respValue{Kind: prefix, String: string(payload[:size])}, nil
	case '*':
		count, err = strconv.Atoi(line)
		if err != nil {
			return respValue{}, fmt.Errorf("parse RESP array length: %w", err)
		}
		if count < 0 {
			return respValue{Kind: prefix}, nil
		}
		values = make([]respValue, 0, count)
		for i = 0; i < count; i++ {
			value, err = readRESPValue(reader)
			if err != nil {
				return respValue{}, err
			}
			values = append(values, value)
		}
		return respValue{Kind: prefix, Array: values}, nil
	}

	return respValue{}, fmt.Errorf("unsupported RESP prefix %q", prefix)
}

func (c *respClient) close() {
	_ = c.conn.Close()
}

func (c *respClient) command(args ...string) (respValue, error) {
	var (
		err error
	)

	if err = c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return respValue{}, err
	}
	if _, err = c.conn.Write(encodeRESPCommand(args)); err != nil {
		return respValue{}, err
	}
	return readRESPValue(c.reader)
}

func dialRESP(address string, password string, timeout time.Duration) (*respClient, error) {
	var (
		conn   net.Conn
		client *respClient
		value  respValue
		err    error
	)

	conn, err = net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}

	client = &respClient{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		timeout: timeout,
	}

	if password != "" {
		value, err = client.command("AUTH", password)
		if err != nil {
			client.close()
			return nil, err
		}
		if !respStringEquals(value, "OK") {
			client.close()
			return nil, fmt.Errorf("AUTH returned %q", respValueString(value))
		}
	}

	return client, nil
}

func respValueString(value respValue) string {
	if value.Kind == ':' {
		return strconv.FormatInt(value.Integer, 10)
	}
	return value.String
}

func respStringEquals(value respValue, expected string) bool {
	return strings.EqualFold(respValueString(value), expected)
}

func respArrayToMap(value respValue) map[string]string {
	var (
		result map[string]string
		i      int
		key    string
	)

	result = make(map[string]string)
	if value.Kind != '*' {
		return result
	}
	for i = 0; i+1 < len(value.Array); i += 2 {
		key = respValueString(value.Array[i])
		result[key] = respValueString(value.Array[i+1])
	}
	return result
}

func respArrayToMaps(value respValue) []map[string]string {
	var (
		result []map[string]string
		i      int
	)

	result = make([]map[string]string, 0)
	if value.Kind != '*' {
		return result
	}
	for i = 0; i < len(value.Array); i++ {
		result = append(result, respArrayToMap(value.Array[i]))
	}
	return result
}

func valkeyAddress(info map[string]string) string {
	return net.JoinHostPort(info["ip"], info["port"])
}

func valkeyFlagsUnhealthy(flags string) bool {
	var (
		parts []string
		i     int
		part  string
	)

	parts = strings.Split(flags, ",")
	for i = 0; i < len(parts); i++ {
		part = strings.TrimSpace(parts[i])
		if part == "s_down" || part == "o_down" || part == "disconnected" {
			return true
		}
	}
	return false
}

func pingValkeyNode(node *valkeyNodeStatus, password string, timeout time.Duration) {
	var (
		client *respClient
		value  respValue
		err    error
	)

	client, err = dialRESP(node.Address, password, timeout)
	if err != nil {
		node.State = "critical"
		node.Status = fmt.Sprintf("Failed to connect: %v", err)
		return
	}
	defer client.close()

	value, err = client.command("PING")
	if err != nil {
		node.State = "critical"
		node.Status = fmt.Sprintf("PING failed: %v", err)
		return
	}
	if !respStringEquals(value, "PONG") {
		node.State = "critical"
		node.Status = fmt.Sprintf("PING returned %q.", respValueString(value))
		return
	}

	node.State = "ok"
	node.Status = "PING returned PONG."
}

func calculateValkeyHealth(config valkeyHealthConfig) (valkeyHealthResponse, error) {
	var (
		response       valkeyHealthResponse
		err            error
		i              int
		sentinelAddr   string
		sentinel       *respClient
		value          respValue
		masterInfo     map[string]string
		replicaInfos   []map[string]string
		node           valkeyNodeStatus
		nodeCount      int
		sentinelStatus valkeySentinelStatus
	)

	response = valkeyHealthResponse{
		State:  "ok",
		Status: "Valkey Sentinel reports the expected primary and replicas healthy.",
		Checks: make(map[string]string),
	}

	if err = validateValkeyHealthConfig(&config); err != nil {
		response.State = "error"
		response.Status = err.Error()
		return response, nil
	}
	response.MasterName = config.MasterName
	response.ExpectedNodes = config.ExpectedNodes

	for i = 0; i < len(config.SentinelAddrs); i++ {
		sentinelAddr = config.SentinelAddrs[i]
		sentinelStatus = valkeySentinelStatus{Address: sentinelAddr, State: "ok", Status: "Connected."}
		sentinel, err = dialRESP(sentinelAddr, config.SentinelPassword, config.Timeout)
		if err != nil {
			response.Sentinels = append(response.Sentinels, valkeySentinelStatus{
				Address: sentinelAddr,
				State:   "error",
				Status:  fmt.Sprintf("Failed to connect: %v", err),
			})
			continue
		}
		defer sentinel.close()

		value, err = sentinel.command("PING")
		if err != nil || !respStringEquals(value, "PONG") {
			sentinelStatus.State = "error"
			if err != nil {
				sentinelStatus.Status = fmt.Sprintf("PING failed: %v", err)
			} else {
				sentinelStatus.Status = fmt.Sprintf("PING returned %q.", respValueString(value))
			}
			response.Sentinels = append(response.Sentinels, sentinelStatus)
			continue
		}

		value, err = sentinel.command("SENTINEL", "CKQUORUM", config.MasterName)
		if err != nil {
			sentinelStatus.State = "critical"
			sentinelStatus.Status = fmt.Sprintf("CKQUORUM failed: %v", err)
			response.Sentinels = append(response.Sentinels, sentinelStatus)
			response.State = "critical"
			response.Status = sentinelStatus.Status
			return response, nil
		}
		response.Checks["ckquorum"] = respValueString(value)

		value, err = sentinel.command("SENTINEL", "MASTER", config.MasterName)
		if err != nil {
			sentinelStatus.State = "critical"
			sentinelStatus.Status = fmt.Sprintf("SENTINEL MASTER failed: %v", err)
			response.Sentinels = append(response.Sentinels, sentinelStatus)
			response.State = "critical"
			response.Status = sentinelStatus.Status
			return response, nil
		}
		masterInfo = respArrayToMap(value)

		value, err = sentinel.command("SENTINEL", "REPLICAS", config.MasterName)
		if err != nil {
			sentinelStatus.State = "critical"
			sentinelStatus.Status = fmt.Sprintf("SENTINEL REPLICAS failed: %v", err)
			response.Sentinels = append(response.Sentinels, sentinelStatus)
			response.State = "critical"
			response.Status = sentinelStatus.Status
			return response, nil
		}
		replicaInfos = respArrayToMaps(value)

		response.Sentinels = append(response.Sentinels, sentinelStatus)
		break
	}

	if masterInfo == nil {
		response.State = "error"
		response.Status = "No configured Valkey Sentinel could provide health data."
		return response, nil
	}

	node = valkeyNodeStatus{
		Role:    "primary",
		Address: valkeyAddress(masterInfo),
		Flags:   masterInfo["flags"],
	}
	response.Nodes = append(response.Nodes, node)
	for i = 0; i < len(replicaInfos); i++ {
		node = valkeyNodeStatus{
			Role:    "replica",
			Address: valkeyAddress(replicaInfos[i]),
			Flags:   replicaInfos[i]["flags"],
		}
		response.Nodes = append(response.Nodes, node)
	}

	nodeCount = len(response.Nodes)
	response.Checks["node_count"] = strconv.Itoa(nodeCount)
	if nodeCount != config.ExpectedNodes {
		response.State = "critical"
		response.Status = fmt.Sprintf("Valkey has %d monitored data node(s), expected %d.", nodeCount, config.ExpectedNodes)
		return response, nil
	}

	for i = 0; i < len(response.Nodes); i++ {
		if response.Nodes[i].Address == ":" {
			response.Nodes[i].State = "critical"
			response.Nodes[i].Status = "Sentinel did not return an IP and port for this node."
			response.State = "critical"
			response.Status = "Sentinel returned incomplete Valkey node data."
			return response, nil
		}
		if valkeyFlagsUnhealthy(response.Nodes[i].Flags) {
			response.Nodes[i].State = "critical"
			response.Nodes[i].Status = fmt.Sprintf("Sentinel flags are unhealthy: %s.", response.Nodes[i].Flags)
			response.State = "critical"
			response.Status = response.Nodes[i].Status
			return response, nil
		}
		pingValkeyNode(&response.Nodes[i], config.ValkeyPassword, config.Timeout)
		if response.Nodes[i].State != "ok" {
			response.State = "critical"
			response.Status = response.Nodes[i].Status
			return response, nil
		}
	}

	return response, nil
}

func (a *App) valkeyHealthHandler(w http.ResponseWriter, r *http.Request) {
	var (
		response   valkeyHealthResponse
		payload    []byte
		err        error
		statusCode int
	)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	response, err = calculateValkeyHealth(a.Config.Valkey)
	if err != nil {
		log.Printf("Error collecting Valkey health: %v", err)
		http.Error(w, "Failed to collect Valkey health", http.StatusInternalServerError)
		return
	}

	payload, err = json.MarshalIndent(response, "", "  ")
	if err != nil {
		log.Printf("Error serializing Valkey health response: %v", err)
		http.Error(w, "Failed to serialize Valkey health response", http.StatusInternalServerError)
		return
	}

	statusCode = http.StatusOK
	if response.State != "ok" {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}
