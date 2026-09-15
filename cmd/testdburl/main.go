package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

func main() {
	testURL, parsed, err := buildTestURL()
	if err != nil {
		fatal("TEST DATABASE URL: FAIL", err)
	}
	productionURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if productionURL == "" {
		fatal("PRODUCTION/TEST SEPARATION: FAIL", fmt.Errorf("DATABASE_URL is missing"))
	}
	production, err := url.Parse(productionURL)
	if err != nil {
		fatal("PRODUCTION/TEST SEPARATION: FAIL", fmt.Errorf("production URL is invalid"))
	}
	if sameDatabase(parsed, production) {
		fatal("UNSAFE INTEGRATION DATABASE CONFIGURATION", fmt.Errorf("test and production database endpoints match"))
	}

	fmt.Println("TEST URL")
	fmt.Println("- valid URI: true")
	fmt.Printf("- public host: %s\n", parsed.Hostname())
	fmt.Printf("- port: %s\n", parsed.Port())
	fmt.Printf("- sslmode: %s\n", parsed.Query().Get("sslmode"))
	fmt.Println("- credentials encoded: true")
	fmt.Println("- production/test separation: VERIFIED")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, testURL)
	if err != nil {
		fatal("TEST DATABASE CONNECTION: FAIL", safeError(err))
	}
	defer db.Close()
	if err := db.Pool.Ping(ctx); err != nil {
		fatal("TEST DATABASE CONNECTION: FAIL", safeError(err))
	}
	fmt.Println("TEST DATABASE CONNECTION: PASS")

	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-tags=integration", "./...")
	cmd.Env = append(os.Environ(), "TEST_DATABASE_URL="+testURL, "ALLOW_INTEGRATION_DB_TESTS=true", "REQUIRE_INTEGRATION_DB=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("INTEGRATION: FAIL", fmt.Errorf("integration command failed"))
	}
	fmt.Println("INTEGRATION: PASS")
}

func buildTestURL() (string, *url.URL, error) {
	username := strings.TrimSpace(os.Getenv("PGUSER"))
	password := os.Getenv("PGPASSWORD")
	databaseName := strings.TrimSpace(os.Getenv("PGDATABASE"))
	host := strings.TrimSpace(os.Getenv("RAILWAY_TCP_PROXY_DOMAIN"))
	port := strings.TrimSpace(os.Getenv("RAILWAY_TCP_PROXY_PORT"))
	missing := make([]string, 0, 5)
	if username == "" {
		missing = append(missing, "PGUSER")
	}
	if password == "" {
		missing = append(missing, "PGPASSWORD")
	}
	if databaseName == "" {
		missing = append(missing, "PGDATABASE")
	}
	if host == "" {
		missing = append(missing, "RAILWAY_TCP_PROXY_DOMAIN")
	}
	if port == "" {
		missing = append(missing, "RAILWAY_TCP_PROXY_PORT")
	}
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("missing Railway variables: %s", strings.Join(missing, ", "))
	}
	parsed := &url.URL{Scheme: "postgresql", Host: net.JoinHostPort(host, port), Path: "/" + databaseName}
	parsed.User = url.UserPassword(username, password)
	query := parsed.Query()
	query.Set("sslmode", "require")
	parsed.RawQuery = query.Encode()
	return parsed.String(), parsed, nil
}

func sameDatabase(testURL, productionURL *url.URL) bool {
	return strings.EqualFold(testURL.Scheme, productionURL.Scheme) &&
		strings.EqualFold(testURL.Hostname(), productionURL.Hostname()) &&
		testURL.Port() == productionURL.Port() &&
		strings.TrimPrefix(testURL.Path, "/") == strings.TrimPrefix(productionURL.Path, "/")
}

func safeError(err error) error {
	message := err.Error()
	for _, marker := range []string{"postgresql://", "postgres://", "password", "PASSWORD"} {
		if idx := strings.Index(message, marker); idx >= 0 {
			message = message[:idx] + "[redacted]"
		}
	}
	return fmt.Errorf("%s", message)
}

func fatal(label string, err error) {
	fmt.Printf("%s: %s\n", label, safeError(err))
	os.Exit(1)
}
