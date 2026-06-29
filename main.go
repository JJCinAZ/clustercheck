package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type App struct {
	DB *sql.DB
}

type mountInfo struct {
	Device     string
	MountPoint string
	FSType     string
	Options    string
}

type storageFilesystemStatus struct {
	Device         string  `json:"device"`
	MountPoint     string  `json:"mount_point"`
	FileSystemType string  `json:"filesystem_type"`
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	State          string  `json:"state"`
	Status         string  `json:"status"`
}

type storageStatusResponse struct {
	State       string                    `json:"state"`
	Status      string                    `json:"status"`
	Filesystems []storageFilesystemStatus `json:"filesystems"`
}

type clusterHealthResponse struct {
	State               string            `json:"state"`
	Status              string            `json:"status"`
	ExpectedClusterSize int               `json:"expected_cluster_size"`
	ClusterSize         int               `json:"cluster_size"`
	Checks              map[string]string `json:"checks"`
}

type valkeyHealthConfig struct {
	SentinelAddrs    []string
	MasterName       string
	ExpectedNodes    int
	Timeout          time.Duration
	SentinelPassword string
	ValkeyPassword   string
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

var ignoredFSTypes = map[string]struct{}{
	"autofs":          {},
	"bpf":             {},
	"cgroup":          {},
	"cgroup2":         {},
	"configfs":        {},
	"debugfs":         {},
	"devpts":          {},
	"devtmpfs":        {},
	"fusectl":         {},
	"hugetlbfs":       {},
	"mqueue":          {},
	"nsfs":            {},
	"proc":            {},
	"pstore":          {},
	"ramfs":           {},
	"rpc_pipefs":      {},
	"securityfs":      {},
	"sysfs":           {},
	"tmpfs":           {},
	"tracefs":         {},
	"binfmt_misc":     {},
	"cgroupfs":        {},
	"squashfs":        {},
	"devfs":           {},
	"fdescfs":         {},
	"linsysfs":        {},
	"linprocfs":       {},
	"rootfs":          {},
	"selinuxfs":       {},
	"fuse.gvfsd-fuse": {},
}

var mountFieldReplacer = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func decodeMountField(field string) string {
	return mountFieldReplacer.Replace(field)
}

func parseMounts(path string) ([]mountInfo, error) {
	var (
		file    *os.File
		err     error
		scanErr error
		scanner *bufio.Scanner
		line    string
		fields  []string
		mounts  []mountInfo
		mount   mountInfo
		seen    map[string]struct{}
		ok      bool
	)

	file, err = os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s was not found on this host", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		_ = file.Close()
	}()

	seen = make(map[string]struct{})
	scanner = bufio.NewScanner(file)
	for scanner.Scan() {
		line = scanner.Text()
		fields = strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		mount = mountInfo{
			Device:     decodeMountField(fields[0]),
			MountPoint: decodeMountField(fields[1]),
			FSType:     fields[2],
			Options:    fields[3],
		}

		if _, ok = ignoredFSTypes[mount.FSType]; ok {
			continue
		}
		if _, ok = seen[mount.MountPoint]; ok {
			continue
		}

		seen[mount.MountPoint] = struct{}{}
		mounts = append(mounts, mount)
	}

	scanErr = scanner.Err()
	if scanErr != nil {
		return nil, fmt.Errorf("read %s: %w", path, scanErr)
	}

	sort.Slice(mounts, func(i int, j int) bool {
		return mounts[i].MountPoint < mounts[j].MountPoint
	})
	return mounts, nil
}

func stateSeverity(state string) int {
	var (
		severity int
	)

	severity = 0
	switch state {
	case "warning":
		severity = 1
	case "critical":
		severity = 2
	case "error":
		severity = 3
	}
	return severity
}

func calculateStorageStatus() (storageStatusResponse, error) {
	var (
		mounts        []mountInfo
		err           error
		response      storageStatusResponse
		stat          syscall.Statfs_t
		fs            storageFilesystemStatus
		state         string
		status        string
		total         uint64
		used          uint64
		available     uint64
		usedPercent   float64
		i             int
		mount         mountInfo
		overallState  string
		overallStatus string
	)

	mounts, err = parseMounts("/proc/self/mounts")
	if err != nil {
		return storageStatusResponse{}, err
	}

	response.Filesystems = make([]storageFilesystemStatus, 0, len(mounts))
	overallState = "ok"
	overallStatus = "All storage filesystems are healthy."

	for i = 0; i < len(mounts); i++ {
		mount = mounts[i]
		err = syscall.Statfs(mount.MountPoint, &stat)
		if err != nil {
			response.Filesystems = append(response.Filesystems, storageFilesystemStatus{
				Device:         mount.Device,
				MountPoint:     mount.MountPoint,
				FileSystemType: mount.FSType,
				State:          "error",
				Status:         fmt.Sprintf("Failed to read filesystem stats: %v", err),
			})
			if stateSeverity("error") > stateSeverity(overallState) {
				overallState = "error"
				overallStatus = "At least one filesystem cannot be inspected."
			}
			continue
		}

		total = stat.Blocks * uint64(stat.Bsize)
		used = (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
		available = stat.Bavail * uint64(stat.Bsize)
		usedPercent = 0
		if total > 0 {
			usedPercent = (float64(used) / float64(total)) * 100
		}

		state = "ok"
		status = fmt.Sprintf("%.2f%% used (%d/%d bytes).", usedPercent, used, total)

		if strings.Contains(","+mount.Options+",", ",ro,") {
			state = "warning"
			status = "Filesystem is mounted read-only."
		} else if usedPercent >= 95 {
			state = "critical"
			status = fmt.Sprintf("Filesystem usage is critical at %.2f%%.", usedPercent)
		} else if usedPercent >= 85 {
			state = "warning"
			status = fmt.Sprintf("Filesystem usage is high at %.2f%%.", usedPercent)
		}

		fs = storageFilesystemStatus{
			Device:         mount.Device,
			MountPoint:     mount.MountPoint,
			FileSystemType: mount.FSType,
			TotalBytes:     total,
			UsedBytes:      used,
			AvailableBytes: available,
			UsedPercent:    usedPercent,
			State:          state,
			Status:         status,
		}
		response.Filesystems = append(response.Filesystems, fs)

		if stateSeverity(state) > stateSeverity(overallState) {
			overallState = state
			if state == "critical" {
				overallStatus = "At least one filesystem has critically high usage."
			} else if state == "warning" {
				overallStatus = "At least one filesystem has elevated usage or is read-only."
			}
		}
	}

	if len(response.Filesystems) == 0 {
		response.State = "error"
		response.Status = "No storage filesystems were discovered."
		return response, nil
	}

	response.State = overallState
	response.Status = overallStatus
	return response, nil
}

func queryStatusVariables(db *sql.DB, variableNames []string) (map[string]string, error) {
	var (
		err           error
		i             int
		variableName  string
		variableValue string
		statuses      map[string]string
	)

	statuses = make(map[string]string)
	for i = 0; i < len(variableNames); i++ {
		if err = db.QueryRow(fmt.Sprintf("SHOW STATUS LIKE '%s';", variableNames[i])).Scan(&variableName, &variableValue); err != nil {
			return nil, err
		}
		statuses[variableName] = variableValue
	}

	return statuses, nil
}

func calculateClusterHealth(db *sql.DB) (clusterHealthResponse, error) {
	var (
		response               clusterHealthResponse
		statuses               map[string]string
		err                    error
		expectedClusterSizeRaw string
		expectedClusterSize    int
		clusterSize            int
		ok                     bool
	)

	response = clusterHealthResponse{
		State:  "ok",
		Status: "All expected cluster members are online and this node is synced.",
		Checks: make(map[string]string),
	}

	expectedClusterSizeRaw = os.Getenv("EXPECTED_CLUSTER_SIZE")
	if expectedClusterSizeRaw == "" {
		response.State = "error"
		response.Status = "EXPECTED_CLUSTER_SIZE is not set, so cluster membership cannot be verified."
		return response, nil
	}

	expectedClusterSize, err = strconv.Atoi(expectedClusterSizeRaw)
	if err != nil || expectedClusterSize < 1 {
		response.State = "error"
		response.Status = "EXPECTED_CLUSTER_SIZE must be a positive integer."
		return response, nil
	}
	response.ExpectedClusterSize = expectedClusterSize

	statuses, err = queryStatusVariables(db, []string{
		"wsrep_cluster_size",
		"wsrep_cluster_status",
		"wsrep_connected",
		"wsrep_local_state",
		"wsrep_ready",
	})
	if err != nil {
		return clusterHealthResponse{}, err
	}
	response.Checks = statuses

	if _, ok = statuses["wsrep_cluster_size"]; !ok {
		response.State = "error"
		response.Status = "wsrep_cluster_size is unavailable."
		return response, nil
	}

	clusterSize, err = strconv.Atoi(statuses["wsrep_cluster_size"])
	if err != nil {
		response.State = "error"
		response.Status = "wsrep_cluster_size is not an integer."
		return response, nil
	}
	response.ClusterSize = clusterSize

	if statuses["wsrep_cluster_status"] != "Primary" {
		response.State = "critical"
		response.Status = "Cluster is not in Primary status."
		return response, nil
	}
	if statuses["wsrep_connected"] != "ON" {
		response.State = "critical"
		response.Status = "Node is not connected to the cluster."
		return response, nil
	}
	if statuses["wsrep_ready"] != "ON" {
		response.State = "critical"
		response.Status = "Node is not ready for queries."
		return response, nil
	}
	if statuses["wsrep_local_state"] != "4" {
		response.State = "critical"
		response.Status = "Node is not synced with the cluster."
		return response, nil
	}
	if clusterSize != expectedClusterSize {
		response.State = "critical"
		response.Status = fmt.Sprintf("Cluster has %d online member(s), expected %d.", clusterSize, expectedClusterSize)
		return response, nil
	}

	return response, nil
}

func envString(name string, defaultValue string) string {
	var (
		value string
	)

	value = os.Getenv(name)
	if value == "" {
		value = defaultValue
	}
	return value
}

func envInt(name string, defaultValue int) (int, error) {
	var (
		valueRaw string
		value    int
		err      error
	)

	valueRaw = os.Getenv(name)
	if valueRaw == "" {
		return defaultValue, nil
	}

	value, err = strconv.Atoi(valueRaw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func loadValkeyHealthConfig() (valkeyHealthConfig, error) {
	var (
		config      valkeyHealthConfig
		addrsRaw    string
		addrParts   []string
		timeoutSecs int
		err         error
		i           int
		addr        string
	)

	addrsRaw = envString("VALKEY_SENTINEL_ADDRS", "127.0.0.1:26379")
	addrParts = strings.Split(addrsRaw, ",")
	config.SentinelAddrs = make([]string, 0, len(addrParts))
	for i = 0; i < len(addrParts); i++ {
		addr = strings.TrimSpace(addrParts[i])
		if addr != "" {
			config.SentinelAddrs = append(config.SentinelAddrs, addr)
		}
	}
	if len(config.SentinelAddrs) == 0 {
		return valkeyHealthConfig{}, fmt.Errorf("VALKEY_SENTINEL_ADDRS must include at least one host:port")
	}

	config.MasterName = envString("VALKEY_MASTER_NAME", "mymaster")
	config.ExpectedNodes, err = envInt("EXPECTED_VALKEY_NODES", 3)
	if err != nil {
		return valkeyHealthConfig{}, err
	}
	timeoutSecs, err = envInt("VALKEY_TIMEOUT_SECONDS", 5)
	if err != nil {
		return valkeyHealthConfig{}, err
	}
	config.Timeout = time.Duration(timeoutSecs) * time.Second
	config.SentinelPassword = os.Getenv("VALKEY_SENTINEL_PASSWORD")
	config.ValkeyPassword = os.Getenv("VALKEY_PASSWORD")

	return config, nil
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

func calculateValkeyHealth() (valkeyHealthResponse, error) {
	var (
		config         valkeyHealthConfig
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

	config, err = loadValkeyHealthConfig()
	if err != nil {
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

func (a *App) clusterCheckHandler(w http.ResponseWriter, r *http.Request) {
	var (
		variableName string
		state        string
		err          error
	)

	err = a.DB.QueryRow("SHOW STATUS LIKE 'wsrep_local_state';").Scan(&variableName, &state)
	if err != nil {
		log.Printf("Error querying database: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	/* wsrep_local_state values:
	Num	Comment			Description
	1	Joining			Node is joining the cluster
	2	Donor/Desynced	Node is the donor to the node joining the cluster
	3	Joined			Node has joined the cluster
	4	Synced			Node is synced with the cluster
	*/
	if state == "4" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Node is synced.")
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "Node is desynced.")
	}
}

func (a *App) clusterHealthHandler(w http.ResponseWriter, r *http.Request) {
	var (
		response   clusterHealthResponse
		payload    []byte
		err        error
		statusCode int
	)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	response, err = calculateClusterHealth(a.DB)
	if err != nil {
		log.Printf("Error collecting cluster health: %v", err)
		http.Error(w, "Failed to collect cluster health", http.StatusInternalServerError)
		return
	}

	payload, err = json.MarshalIndent(response, "", "  ")
	if err != nil {
		log.Printf("Error serializing cluster health response: %v", err)
		http.Error(w, "Failed to serialize cluster health response", http.StatusInternalServerError)
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

	response, err = calculateValkeyHealth()
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

func (a *App) storageStatusHandler(w http.ResponseWriter, r *http.Request) {
	var (
		response   storageStatusResponse
		payload    []byte
		err        error
		statusCode int
	)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	response, err = calculateStorageStatus()
	if err != nil {
		log.Printf("Error collecting storage status: %v", err)
		http.Error(w, "Failed to collect storage status", http.StatusInternalServerError)
		return
	}

	payload, err = json.MarshalIndent(response, "", "  ")
	if err != nil {
		log.Printf("Error serializing storage status response: %v", err)
		http.Error(w, "Failed to serialize storage status response", http.StatusInternalServerError)
		return
	}

	statusCode = http.StatusOK
	if response.State == "critical" || response.State == "error" {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}

func initDB() (*sql.DB, error) {
	var (
		dsn string
		db  *sql.DB
		err error
	)

	dsn = os.Getenv("DSN")
	if dsn == "" {
		return nil, fmt.Errorf(`DSN environment variable not set.  Expect: DSN="user:password@tcp(127.0.0.1:3306)/"`)
	}
	log.Printf("Using DSN: %s", dsn)
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("unable to initially connect to database (may occur if database server is not yet running): %w", err)
	}
	return db, nil
}

func main() {
	var (
		app App
		db  *sql.DB
		err error
		mux *http.ServeMux
	)

	db, err = initDB()
	if err != nil {
		log.Fatal(err)
	}
	app.DB = db

	mux = http.NewServeMux()
	mux.HandleFunc("/", app.clusterCheckHandler)
	mux.HandleFunc("/cluster", app.clusterHealthHandler)
	mux.HandleFunc("/storage", app.storageStatusHandler)
	mux.HandleFunc("/valkey", app.valkeyHealthHandler)

	log.Println("Starting clustercheck server on :8080")
	err = http.ListenAndServe(":8080", mux)
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}
