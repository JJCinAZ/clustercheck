package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jjcinaz/serverconfig"
)

const (
	databaseMaxOpenConnections = 10
	databaseConnectionLifetime = 3 * time.Minute
	databaseConnectionIdleTime = time.Minute
)

type App struct {
	DB         *sql.DB
	Config     clustercheckConfig
	HTTPClient *http.Client
}

func initDB(config serverconfig.MySQLDatabase) (*sql.DB, error) {
	var (
		db  *sql.DB
		err error
	)

	if db, err = sql.Open("mysql", config.ConnectString); err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(databaseMaxOpenConnections)
	db.SetMaxIdleConns(databaseMaxOpenConnections)
	db.SetConnMaxLifetime(databaseConnectionLifetime)
	db.SetConnMaxIdleTime(databaseConnectionIdleTime)
	return db, nil
}

func main() {
	var (
		app    App
		config clustercheckConfig
		db     *sql.DB
		err    error
		mux    *http.ServeMux
	)

	if config, err = loadConfig(configPath()); err != nil {
		log.Fatal(err)
	}
	if db, err = initDB(config.Database); err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	log.Printf("Using database server: %s", config.Database.Server)

	app.DB = db
	app.Config = config
	app.HTTPClient = &http.Client{Timeout: config.Proxmox.Timeout}

	mux = http.NewServeMux()
	mux.HandleFunc("/", app.clusterCheckHandler)
	mux.HandleFunc("/cluster", app.clusterHealthHandler)
	mux.HandleFunc("/storage", app.storageStatusHandler)
	mux.HandleFunc("/valkey", app.valkeyHealthHandler)

	if err = app.serve(mux); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}
