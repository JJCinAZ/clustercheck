package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type proxmoxCephStatus struct {
	State  string            `json:"state"`
	Status string            `json:"status"`
	Health string            `json:"health"`
	Checks map[string]string `json:"checks,omitempty"`
}

type proxmoxDriveStatus struct {
	Node        string `json:"node"`
	Device      string `json:"device"`
	Model       string `json:"model,omitempty"`
	Serial      string `json:"serial,omitempty"`
	Type        string `json:"type,omitempty"`
	SmartHealth string `json:"smart_health,omitempty"`
	State       string `json:"state"`
	Status      string `json:"status"`
}

type proxmoxAPIResponse struct {
	Data json.RawMessage `json:"data"`
}

type proxmoxStatusResponse struct {
	Ceph   proxmoxCephStatus    `json:"ceph"`
	Drives []proxmoxDriveStatus `json:"drives"`
}

func proxmoxAuthorization(token string) string {
	if strings.HasPrefix(token, "PVEAPIToken=") {
		return token
	}
	return "PVEAPIToken=" + token
}

func proxmoxAPIGet(ctx context.Context, client *http.Client, config proxmoxConfig, apiPath string, query url.Values, target any) error {
	var (
		requestURL string
		req        *http.Request
		resp       *http.Response
		decoder    *json.Decoder
		wrapper    proxmoxAPIResponse
		body       []byte
		err        error
	)

	requestURL = config.Endpoint + "/api2/json" + apiPath
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", proxmoxAuthorization(config.APIToken))
	req.Header.Set("Accept", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Proxmox API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	decoder = json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err = decoder.Decode(&wrapper); err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	if err = json.Unmarshal(wrapper.Data, target); err != nil {
		return err
	}
	return nil
}

func valueString(value any) string {
	var (
		number json.Number
		ok     bool
	)

	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%v", typed)
	}
	number, ok = value.(json.Number)
	if ok {
		return number.String()
	}
	return fmt.Sprintf("%v", value)
}

func mapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	return valueString(values[key])
}

func mapBool(values map[string]any, key string) bool {
	var (
		raw    string
		number json.Number
	)

	if values == nil {
		return false
	}
	switch typed := values[key].(type) {
	case bool:
		return typed
	case json.Number:
		number = typed
		raw = number.String()
	case string:
		raw = typed
	default:
		raw = valueString(typed)
	}
	return raw == "1" || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "yes") || strings.EqualFold(raw, "online")
}

func mapObject(values map[string]any, key string) map[string]any {
	var (
		result map[string]any
		ok     bool
	)

	if values == nil {
		return nil
	}
	result, ok = values[key].(map[string]any)
	if !ok {
		return nil
	}
	return result
}

func cephHealthState(health string) string {
	switch strings.ToUpper(strings.TrimSpace(health)) {
	case "HEALTH_OK", "OK":
		return "ok"
	case "HEALTH_WARN", "WARN", "WARNING":
		return "warning"
	case "HEALTH_ERR", "ERR", "ERROR":
		return "critical"
	}
	return "error"
}

func collectCephChecks(health map[string]any) map[string]string {
	var (
		checksRaw map[string]any
		checks    map[string]string
		key       string
		value     any
		item      map[string]any
		summary   map[string]any
		message   string
		ok        bool
	)

	checksRaw = mapObject(health, "checks")
	if len(checksRaw) == 0 {
		return nil
	}

	checks = make(map[string]string)
	for key, value = range checksRaw {
		item, ok = value.(map[string]any)
		if !ok {
			checks[key] = valueString(value)
			continue
		}
		summary = mapObject(item, "summary")
		message = mapString(summary, "message")
		if message == "" {
			message = mapString(item, "severity")
		}
		if message == "" {
			message = valueString(value)
		}
		checks[key] = message
	}
	return checks
}

func parseProxmoxCephStatus(data map[string]any) proxmoxCephStatus {
	var (
		status    proxmoxCephStatus
		healthMap map[string]any
		health    string
	)

	healthMap = mapObject(data, "health")
	health = mapString(healthMap, "status")
	if health == "" {
		health = mapString(data, "health")
	}
	if health == "" {
		health = mapString(data, "overall_status")
	}

	status = proxmoxCephStatus{
		State:  cephHealthState(health),
		Health: health,
		Checks: collectCephChecks(healthMap),
	}
	if status.State == "ok" {
		status.Status = fmt.Sprintf("Ceph health is %s.", health)
	} else if health == "" {
		status.Status = "Ceph health status was not present in the Proxmox response."
	} else {
		status.Status = fmt.Sprintf("Ceph health is %s.", health)
	}
	return status
}

func proxmoxCephStatusCheck(ctx context.Context, client *http.Client, config proxmoxConfig) (proxmoxCephStatus, error) {
	var (
		data map[string]any
		err  error
	)

	if err = proxmoxAPIGet(ctx, client, config, "/cluster/ceph/status", nil, &data); err != nil {
		return proxmoxCephStatus{}, err
	}
	return parseProxmoxCephStatus(data), nil
}

func proxmoxClusterNodes(ctx context.Context, client *http.Client, config proxmoxConfig) ([]string, []proxmoxDriveStatus, error) {
	var (
		rows          []map[string]any
		nodes         []string
		nodeErrors    []proxmoxDriveStatus
		err           error
		i             int
		row           map[string]any
		nodeName      string
		resourceType  string
		resourceState string
	)

	if err = proxmoxAPIGet(ctx, client, config, "/cluster/status", nil, &rows); err != nil {
		return nil, nil, err
	}

	nodes = make([]string, 0)
	for i = 0; i < len(rows); i++ {
		row = rows[i]
		resourceType = mapString(row, "type")
		if resourceType != "node" {
			continue
		}
		nodeName = mapString(row, "name")
		if nodeName == "" {
			nodeName = mapString(row, "node")
		}
		if nodeName == "" {
			continue
		}
		if !mapBool(row, "online") {
			resourceState = mapString(row, "status")
			if resourceState == "" {
				resourceState = "offline"
			}
			nodeErrors = append(nodeErrors, proxmoxDriveStatus{
				Node:   nodeName,
				State:  "error",
				Status: fmt.Sprintf("Proxmox node is not online: %s.", resourceState),
			})
			continue
		}
		nodes = append(nodes, nodeName)
	}
	return nodes, nodeErrors, nil
}

func proxmoxDiskDevice(row map[string]any) string {
	var device string

	device = mapString(row, "devpath")
	if device == "" {
		device = mapString(row, "device")
	}
	if device == "" {
		device = mapString(row, "path")
	}
	return device
}

func proxmoxDriveFromDiskRow(node string, row map[string]any) proxmoxDriveStatus {
	return proxmoxDriveStatus{
		Node:   node,
		Device: proxmoxDiskDevice(row),
		Model:  mapString(row, "model"),
		Serial: mapString(row, "serial"),
		Type:   mapString(row, "type"),
	}
}

func smartHealthFromData(data map[string]any) string {
	var health string

	health = mapString(data, "health")
	if health == "" {
		health = mapString(data, "smart_status")
	}
	if health == "" {
		health = mapString(data, "passed")
	}
	if health == "" {
		health = mapString(mapObject(data, "smart_status"), "passed")
	}
	return health
}

func smartState(health string) string {
	if strings.EqualFold(health, "PASSED") || strings.EqualFold(health, "OK") || strings.EqualFold(health, "true") || health == "1" {
		return "ok"
	}
	if health == "" {
		return "error"
	}
	return "critical"
}

func proxmoxDriveSMART(ctx context.Context, client *http.Client, config proxmoxConfig, node string, drive proxmoxDriveStatus) proxmoxDriveStatus {
	var (
		query  url.Values
		data   map[string]any
		health string
		err    error
	)

	if drive.Device == "" {
		drive.State = "error"
		drive.Status = "Proxmox disk entry did not include a device path."
		return drive
	}

	query = url.Values{}
	query.Set("disk", drive.Device)
	if err = proxmoxAPIGet(ctx, client, config, "/nodes/"+url.PathEscape(node)+"/disks/smart", query, &data); err != nil {
		drive.State = "error"
		drive.Status = fmt.Sprintf("SMART query failed: %v", err)
		return drive
	}

	health = smartHealthFromData(data)
	drive.SmartHealth = health
	drive.State = smartState(health)
	if drive.State == "ok" {
		drive.Status = fmt.Sprintf("SMART health is %s.", health)
	} else if health == "" {
		drive.Status = "SMART health status was not present in the Proxmox response."
	} else {
		drive.Status = fmt.Sprintf("SMART health is %s.", health)
	}
	return drive
}

func proxmoxNodeDrives(ctx context.Context, client *http.Client, config proxmoxConfig, node string) []proxmoxDriveStatus {
	var (
		rows   []map[string]any
		drives []proxmoxDriveStatus
		drive  proxmoxDriveStatus
		err    error
		i      int
	)

	if err = proxmoxAPIGet(ctx, client, config, "/nodes/"+url.PathEscape(node)+"/disks/list", nil, &rows); err != nil {
		return []proxmoxDriveStatus{{
			Node:   node,
			State:  "error",
			Status: fmt.Sprintf("Disk list query failed: %v", err),
		}}
	}

	drives = make([]proxmoxDriveStatus, 0, len(rows))
	for i = 0; i < len(rows); i++ {
		drive = proxmoxDriveFromDiskRow(node, rows[i])
		drives = append(drives, proxmoxDriveSMART(ctx, client, config, node, drive))
	}
	if len(drives) == 0 {
		drives = append(drives, proxmoxDriveStatus{
			Node:   node,
			State:  "warning",
			Status: "No Proxmox disks were reported for this node.",
		})
	}
	return drives
}

func checkProxmoxStorage(ctx context.Context, client *http.Client, config proxmoxConfig) (proxmoxStatusResponse, error) {
	var (
		response   proxmoxStatusResponse
		nodeErrors []proxmoxDriveStatus
		nodes      []string
		err        error
		i          int
		drives     []proxmoxDriveStatus
	)

	response.Ceph, err = proxmoxCephStatusCheck(ctx, client, config)
	if err != nil {
		return proxmoxStatusResponse{}, err
	}

	nodes, nodeErrors, err = proxmoxClusterNodes(ctx, client, config)
	if err != nil {
		return proxmoxStatusResponse{}, err
	}
	response.Drives = append(response.Drives, nodeErrors...)
	for i = 0; i < len(nodes); i++ {
		drives = proxmoxNodeDrives(ctx, client, config, nodes[i])
		response.Drives = append(response.Drives, drives...)
	}
	return response, nil
}
