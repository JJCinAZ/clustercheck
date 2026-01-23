package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

type App struct {
	DB *sql.DB
}

func (a *App) clusterCheckHandler(w http.ResponseWriter, r *http.Request) {
	var variableName, state string
	err := a.DB.QueryRow("SHOW STATUS LIKE 'wsrep_local_state';").Scan(&variableName, &state)
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

func initDB() (*sql.DB, error) {
	dsn := os.Getenv("DSN")
	if dsn == "" {
		return nil, fmt.Errorf(`DSN environment variable not set.  Expect: DSN="user:password@tcp(127.0.0.1:3306)/"`)
	}
	log.Printf("Using DSN: %s", dsn)
	db, err := sql.Open("mysql", dsn)
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
	var app App

	if db, err := initDB(); err != nil {
		log.Fatal(err)
	} else {
		app.DB = db
	}
	http.HandleFunc("/", app.clusterCheckHandler)
	log.Println("Starting clustercheck server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}
