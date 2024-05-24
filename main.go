package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/magiconair/properties"
	"github.com/teris-io/shortid"
)

var db *sql.DB
var dbMutex sync.Mutex
var domain string
var adminKey string

type ShortURL struct {
	ID        int64
	ShortCode string
	LongURL   string
}

func initDB(db *sql.DB) error {
	createTableSQL := `
		CREATE TABLE IF NOT EXISTS short_urls (
        id INT AUTO_INCREMENT PRIMARY KEY,
        short_code VARCHAR(255) UNIQUE NOT NULL,
        long_url TEXT NOT NULL
    );
    `

	_, err := db.Exec(createTableSQL)
	return err
}

func main() {
	p := properties.MustLoadFile("config.properties", properties.UTF8)

	dbDriver := p.GetString("db.driver", "mysql")
	dbUser := p.GetString("db.user", "root")
	dbPass := p.GetString("db.password", "")
	dbHost := p.GetString("db.host", "localhost")
	dbPort := p.GetInt("db.port", 3306)
	dbName := p.GetString("db.name", "shorter")

	dbSource := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", dbUser, dbPass, dbHost, dbPort, dbName)

	var err error
	db, err = sql.Open(dbDriver, dbSource)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Database connection failed: %v\n", err)
	}

	if err := initDB(db); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handleShortURL)
	http.HandleFunc("/api/create", handleCreateShortURL)
	http.HandleFunc("/api/delete/", handleDeleteShortURL)

	port := fmt.Sprintf("%d", p.GetInt("main.port", 8080))
	domain = p.GetString("main.domain", "http://localhost")
	adminKey = p.GetString("admin.key", "DEFAULT_KEY")

	server := &http.Server{Addr: ":" + port, Handler: nil}
	go func() {
		log.Printf("Server listening on port %s...\n", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
}

func handleShortURL(w http.ResponseWriter, r *http.Request) {
	shortCode := strings.TrimPrefix(r.URL.Path, "/")
	if shortCode == "" {
		http.NotFound(w, r)
		return
	}

	var shortURL ShortURL
	err := db.QueryRow("SELECT id, short_code, long_url FROM short_urls WHERE short_code = ?", shortCode).Scan(&shortURL.ID, &shortURL.ShortCode, &shortURL.LongURL)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	http.Redirect(w, r, shortURL.LongURL, http.StatusMovedPermanently)
}

func handleCreateShortURL(w http.ResponseWriter, r *http.Request) {
	longURL := r.FormValue("long_url")
	if longURL == "" {
		http.Error(w, "Missing long_url parameter", http.StatusBadRequest)
		return
	}

	finalURL, err := getFinalURL(longURL)
	if err != nil {
		http.Error(w, "Failed to resolve redirection", http.StatusInternalServerError)
		return
	}

	cleanURL := removeQueryParams(finalURL)
	shortCode := generateShortCode()

	dbMutex.Lock()
	defer dbMutex.Unlock()

	var existingShortCode string
	err = db.QueryRow("SELECT short_code FROM short_urls WHERE long_url = ?", cleanURL).Scan(&existingShortCode)
	if err == nil {
		fmt.Fprintf(w, "Short URL already exists: %s\n", domain+existingShortCode)
		return
	} else if err != sql.ErrNoRows {
		http.Error(w, "Failed to check for existing short URL", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec("INSERT INTO short_urls (short_code, long_url) VALUES (?, ?)", shortCode, cleanURL)
	if err != nil {
		http.Error(w, "Failed to create short URL", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Short URL created: %s\n", domain+shortCode)
}

func handleDeleteShortURL(w http.ResponseWriter, r *http.Request) {
	shortCode := strings.TrimPrefix(r.URL.Path, "/api/delete/")
	key := r.Header.Get("Authorization")
	if key != adminKey {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if shortCode == "" {
		http.NotFound(w, r)
		return
	}

	_, err := db.Exec("DELETE FROM short_urls WHERE short_code = ?", shortCode)
	if err != nil {
		http.Error(w, "Failed to delete short URL", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Short URL deleted: %s\n", domain+shortCode)
}

func generateShortCode() string {
	return shortid.MustGenerate()
}

func getFinalURL(urlStr string) (string, error) {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Head(urlStr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if isRedirect(resp.StatusCode) {
		locationURL, err := resp.Location()
		if err != nil {
			return "", err
		}
		return locationURL.String(), nil
	}

	return urlStr, nil
}

func isRedirect(statusCode int) bool {
	return statusCode >= 300 && statusCode <= 399
}

func removeQueryParams(urlStr string) string {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return urlStr
	}

	parsedURL.RawQuery = ""
	return parsedURL.String()
}
