package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
)

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
	Ceph        *proxmoxCephStatus        `json:"ceph,omitempty"`
	Drives      []proxmoxDriveStatus      `json:"drives,omitempty"`
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

func checkStorageSpace() (storageStatusResponse, error) {
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

func updateStorageOverall(response *storageStatusResponse, state string, status string) {
	if stateSeverity(state) <= stateSeverity(response.State) {
		return
	}
	response.State = state
	response.Status = status
}

func addProxmoxStorageStatus(ctx context.Context, response *storageStatusResponse, client *http.Client, config proxmoxConfig) {
	var (
		proxmoxStatus proxmoxStatusResponse
		err           error
		i             int
	)

	if !config.Enabled {
		return
	}
	if client == nil {
		client = http.DefaultClient
	}

	proxmoxStatus, err = checkProxmoxStorage(ctx, client, config)
	if err != nil {
		response.Ceph = &proxmoxCephStatus{
			State:  "error",
			Status: fmt.Sprintf("Failed to collect Proxmox storage status: %v", err),
		}
		updateStorageOverall(response, "error", "Failed to collect Proxmox storage status.")
		return
	}

	response.Ceph = &proxmoxStatus.Ceph
	response.Drives = proxmoxStatus.Drives
	updateStorageOverall(response, proxmoxStatus.Ceph.State, proxmoxStatus.Ceph.Status)
	for i = 0; i < len(proxmoxStatus.Drives); i++ {
		updateStorageOverall(response, proxmoxStatus.Drives[i].State, proxmoxStatus.Drives[i].Status)
	}
}

func checkStorageStatus(ctx context.Context, client *http.Client, config proxmoxConfig) (storageStatusResponse, error) {
	var (
		response storageStatusResponse
		err      error
	)
	response, err = checkStorageSpace()
	if err != nil {
		return storageStatusResponse{}, err
	}
	addProxmoxStorageStatus(ctx, &response, client, config)
	return response, nil
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

	response, err = checkStorageStatus(r.Context(), a.HTTPClient, a.Config.Proxmox)
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
