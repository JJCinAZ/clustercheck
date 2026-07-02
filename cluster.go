package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

type clusterHealthResponse struct {
	State               string            `json:"state"`
	Status              string            `json:"status"`
	ExpectedClusterSize int               `json:"expected_cluster_size"`
	ClusterSize         int               `json:"cluster_size"`
	Checks              map[string]string `json:"checks"`
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

func calculateClusterHealth(db *sql.DB, expectedClusterSize int) (clusterHealthResponse, error) {
	var (
		response    clusterHealthResponse
		statuses    map[string]string
		err         error
		clusterSize int
		ok          bool
	)

	response = clusterHealthResponse{
		State:  "ok",
		Status: "All expected cluster members are online and this node is synced.",
		Checks: make(map[string]string),
	}

	if expectedClusterSize == 0 {
		response.State = "error"
		response.Status = "cluster.expected_size is not set, so cluster membership cannot be verified."
		return response, nil
	}

	if expectedClusterSize < 1 {
		response.State = "error"
		response.Status = "cluster.expected_size must be a positive integer."
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

	response, err = calculateClusterHealth(a.DB, a.Config.Cluster.ExpectedSize)
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
