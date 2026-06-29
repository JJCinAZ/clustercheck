# Galera Clustercheck Service

This Go service provides an HTTP endpoint to check the status of a Galera cluster node by querying its `wsrep_local_state`. It returns an HTTP 200 (OK) if the node is synced (`wsrep_local_state` is `4`), and an HTTP 503 (Service Unavailable) if it is desynced. 
This is intended to be used with load balancers like HAProxy and in place of a cluster-check bash script executed by `xinetd` or `systemd-socket-proxyd`.

## Endpoints

### `GET /`

Checks Galera node sync state:

* `200 OK`: node is synced (`wsrep_local_state=4`)
* `503 Service Unavailable`: node is not synced

### `GET /cluster`

Checks overall Galera cluster health:

* `200 OK`: cluster is Primary, this node is connected/ready/synced, and `wsrep_cluster_size` matches `EXPECTED_CLUSTER_SIZE`
* `503 Service Unavailable`: cluster status is unhealthy, this node is not synced/ready/connected, or the online member count does not match `EXPECTED_CLUSTER_SIZE`
* `500 Internal Server Error`: cluster status data collection failed

The payload includes `state`, `status`, `expected_cluster_size`, `cluster_size`, and the checked `wsrep_*` status variables.

### `GET /storage`

Returns storage/filesystem state and status as JSON.  
The payload includes an overall `state`/`status` plus per-filesystem entries with mount point, type, capacity, utilization, and per-filesystem `state`/`status`.

Response codes:

* `200 OK`: storage checks are healthy or warning-level
* `503 Service Unavailable`: one or more filesystems are critical/error
* `500 Internal Server Error`: storage data collection failed

### `GET /valkey`

Checks a Valkey primary/replica deployment monitored by Valkey Sentinel:

* `200 OK`: Sentinel quorum is OK, the expected data-node count is present, Sentinel does not report down/disconnected node flags, and every reported data node responds to `PING`
* `503 Service Unavailable`: Sentinel cannot verify quorum, node count is wrong, Sentinel reports an unhealthy node flag, or a data node does not respond to `PING`
* `500 Internal Server Error`: Valkey health data collection failed

The payload includes `state`, `status`, `master_name`, `expected_nodes`, `nodes`, `sentinels`, and `checks`.

## Prerequisites

*   Go (version 1.25 or later recommended)
*   MariaDB/MySQL/Percona server with Galera configured
*   Access to a user with `SHOW STATUS` privileges on the database

## Setup and Installation

Follow these steps to set up and run the Galera Clustercheck Service on a Linux host with SystemD

### 1. Get project from Github

```bash
git clone https://github.com/jjc/clustercheck.git
```

### 2. Compile the Go Application

First, compile your Go application into a single executable binary. From your project directory, run the following command:

```bash
GOOS=linux GOARCH=amd64 go build
```

This will create a Linux AMD64 executable file named `clustercheck` in your current directory.  If you are using ARM64, adjust the GOARCH appropriateely.
Copy the executable up to the target Linux host if necessary along with the `clustercheck.service` file.

### 3. Create User and Group

It's a good security practice to run services as a non-root user. Create a dedicated user and group for the service:

```bash
sudo groupadd clustercheck
sudo useradd -r -g clustercheck -s /usr/sbin/nologin clustercheck
```

### 4. Create Directories and Copy Files

Create the necessary directories and copy the compiled binary to the appropriate location:

```bash
sudo mkdir -p /opt/clustercheck
sudo chown clustercheck:clustercheck /opt/clustercheck
sudo cp clustercheck /opt/clustercheck/
```

### 5. Create Configuration File

Create the configuration file that will store environment variables used to configure the service.

```bash
sudo mkdir -p /etc/clustercheck
sudo touch /etc/clustercheck/config
sudo chown -R clustercheck:clustercheck /etc/clustercheck
```

Now, edit the configuration file to add your DSN. Replace the placeholder with your actual MariaDB/MySQL connection string.

```bash
sudo nano /etc/clustercheck/config
```

Add the following line to the file:

```
DSN="cluster_monitor:password@tcp(127.0.0.1:3306)/"
EXPECTED_CLUSTER_SIZE="3"
VALKEY_SENTINEL_ADDRS="127.0.0.1:26379"
VALKEY_MASTER_NAME="mymaster"
EXPECTED_VALKEY_NODES="3"
VALKEY_TIMEOUT_SECONDS="5"
```

If your Valkey deployment requires authentication, also set:

```
VALKEY_SENTINEL_PASSWORD="sentinel-password"
VALKEY_PASSWORD="valkey-password"
```

Create the user in the database:

```mysql
CREATE USER IF NOT EXISTS 'cluster_monitor'@'localhost' IDENTIFIED BY 'password';
GRANT PROCESS ON *.* TO 'cluster_monitor'@'localhost';
GRANT REPLICATION CLIENT ON *.* TO 'cluster_monitor'@'localhost';
FLUSH PRIVILEGES;
```

### 6. Install the systemd Service File

Copy the `clustercheck.service` file (which should be in your project directory) to the `systemd` services directory:

```bash
sudo cp clustercheck.service /etc/systemd/system/
```

### 7. Reload systemd and Start the Service

Reload the `systemd` daemon to recognize the new service definition, then enable it to start on boot and finally start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable clustercheck.service
sudo systemctl start clustercheck.service
```

### 8. Check the Service Status

You can verify that the service is running correctly and check its status:

```bash
sudo systemctl status clustercheck.service
```

To view the service logs, use `journalctl`:

```bash
sudo journalctl -u clustercheck.service
```

## Usage with HAProxy

Once the service is running, you can configure HAProxy to use this endpoint for health checks. Here's an example HAProxy configuration snippet:

```
listen galera_cluster
    bind *:3306
    mode tcp
    option tcplog
    balance leastconn
    option httpchk GET /
    http-check expect status 200
    server node1 192.168.91.11:3306 check port 8080 inter 5s fastinter 2s downinter 30s rise 2 fall 3
    server node2 192.168.91.12:3306 check port 8080 inter 5s fastinter 2s downinter 30s rise 2 fall 3
    server node3 192.168.91.13:3306 check port 8080 inter 5s fastinter 2s downinter 30s rise 2 fall 3
```

> **Note:** Replace `192.168.1.10x` with the actual IP addresses of your database nodes.
