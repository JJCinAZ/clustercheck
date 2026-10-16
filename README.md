# Galera Clustercheck Service

This Go service provides HTTP health endpoints and separate plain TCP health ports for Galera and Valkey nodes. It supports load balancers such as Traefik and HAProxy, replacing cluster-check scripts executed by `xinetd` or `systemd-socket-proxyd`.

## TCP health checks

The service listens on two additional ports by default. Connect without sending a request; it checks the backend, writes a single line on success, then closes the connection. These are plain TCP responses with no HTTP headers.

| Port | Success response | Requirements |
| --- | --- | --- |
| 9200 | `OK\r\n` | Galera is Primary, connected, synced (`wsrep_local_state=4`), ready, has replication enabled (`wsrep_on=ON`), has `read_only=OFF`, has `super_read_only=OFF` if supported, and has `wsrep_reject_queries=NONE`. |
| 9201 | `role:master\r\n` | The node at `valkey.address` returns `master` from `ROLE`. Uses `valkey.password` for authentication if configured. |

An unhealthy node, backend error, or timeout closes the connection without a success response. Every connection performs a fresh check. `tcp.timeout` bounds the entire backend check and response (default 2 seconds); each listener permits at most 32 concurrent checks and closes excess connections.

The Galera TCP check measures this node's write readiness through status and configuration; it does not issue a test write or require the full `cluster.expected_size` to be online. A healthy Primary component can continue serving writes during a member outage. The Valkey TCP check reads the target node's own role, independently of the cluster-wide Sentinel checks on `/valkey`; it does not provide Sentinel consensus or fence an isolated former master.

Run clustercheck on each backend host, pointing `database.server` and `valkey.address` at the corresponding data nodes. `valkey.address` defaults to `127.0.0.1:6379`; it must point directly to a data node, rather than a Sentinel or load balancer. The database settings and HTTP routes continue to work as before.

Configure listeners under `tcp:` as shown below. Explicitly set either bind address to `""` to disable that listener. Environment overrides are `GALERA_TCP_BIND_ADDRESS`, `VALKEY_TCP_BIND_ADDRESS`, `TCP_HEALTH_TIMEOUT`, and `VALKEY_ADDRESS`.

To check manually:

```bash
nc -w 3 127.0.0.1 9200  # OK on a writable Galera node
nc -w 3 127.0.0.1 9201  # role:master on a Valkey master
```

### Traefik configuration

Use the health port as `healthCheck.port` while keeping each backend's actual data port in `servers`. Set `expect` so that closing an unhealthy connection fails the check; a connection-only check would still succeed when the health listener accepts it. No `send` payload is needed. See [Traefik TCP health checks](https://doc.traefik.io/traefik/reference/routing-configuration/tcp/service/#health-check).

Add these services to your Traefik dynamic configuration and reference them from your TCP routers:

```yaml
tcp:
  services:
    galera:
      loadBalancer:
        servers:
          - address: "192.168.91.11:3306"
          - address: "192.168.91.12:3306"
          - address: "192.168.91.13:3306"
        healthCheck:
          port: 9200
          expect: "OK"
          interval: 5s
          timeout: 3s
    valkey-master:
      loadBalancer:
        servers:
          - address: "192.168.91.11:6379"
          - address: "192.168.91.12:6379"
          - address: "192.168.91.13:6379"
        healthCheck:
          port: 9201
          expect: "role:master"
          interval: 5s
          timeout: 3s
```

Set Traefik's timeout higher than `tcp.timeout` to leave time for the response. Allow Traefik to reach the configured health ports on every backend host.

## Endpoints

### `GET /`

Checks Galera node sync state:

* `200 OK`: node is synced (`wsrep_local_state=4`)
* `503 Service Unavailable`: node is not synced

### `GET /cluster`

Checks overall Galera cluster health:

* `200 OK`: cluster is Primary, this node is connected/ready/synced, and `wsrep_cluster_size` matches `cluster.expected_size`
* `503 Service Unavailable`: cluster status is unhealthy, this node is not synced/ready/connected, or the online member count does not match `cluster.expected_size`
* `500 Internal Server Error`: cluster status data collection failed

The payload includes `state`, `status`, `expected_cluster_size`, `cluster_size`, and the checked `wsrep_*` status variables.

### `GET /storage`

Returns storage/filesystem state and status as JSON.  
The payload includes an overall `state`/`status` plus per-filesystem entries with mount point, type, capacity, utilization, and per-filesystem `state`/`status`.
When Proxmox is configured, the same response also includes Ceph cluster health from the Proxmox API and SMART status for all drives reported by all online Proxmox nodes.

Response codes:

* `200 OK`: storage checks are healthy or warning-level
* `503 Service Unavailable`: one or more filesystems, Ceph checks, or SMART checks are critical/error
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

Create the YAML configuration file used to configure the service.

```bash
sudo mkdir -p /etc/clustercheck
sudo touch /etc/clustercheck/config.yaml
sudo chown -R clustercheck:clustercheck /etc/clustercheck
```

Now, edit the configuration file. Replace the placeholder values with your actual MariaDB/MySQL connection settings.

```bash
sudo nano /etc/clustercheck/config.yaml
```

Add the following YAML to the file:

```yaml
http:
  bind_address: ":8080"

tcp:
  galera_bind_address: ":9200"
  valkey_bind_address: ":9201"
  timeout: 2s

database:
  server: "127.0.0.1:3306"
  user: "cluster_monitor"
  password: "password"
  db: ""

cluster:
  expected_size: 3

valkey:
  address: "127.0.0.1:6379"
  sentinel_addrs:
    - "127.0.0.1:26379"
  master_name: "mymaster"
  expected_nodes: 3
  timeout: 5s

proxmox:
  endpoint: "https://p1.acme.com:8006"
  api_token: "root@pam!clustercheck=token-secret"
  timeout: 10s
```

If your Valkey deployment requires authentication, also add these fields under the existing `valkey:` section:

```yaml
  sentinel_password: "sentinel-password"
  password: "valkey-password"
```

If you already have a MySQL connection string, you can use `database.connect_string` instead of the structured `database.server`, `database.user`, `database.password`, and `database.db` fields. The service reads `/etc/clustercheck/config.yaml` by default; set the `CONFIG` environment variable to point at a different file.

The Proxmox API token is sent as an API token authorization header. You may include either the full `PVEAPIToken=...` value or just the token portion, such as `root@pam!clustercheck=token-secret`. When Proxmox is configured, `/storage` queries:

* `/api2/json/cluster/ceph/status`
* `/api2/json/cluster/status`
* `/api2/json/nodes/{node}/disks/list`
* `/api2/json/nodes/{node}/disks/smart?disk={device}`

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
